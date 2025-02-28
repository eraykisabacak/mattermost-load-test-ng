// Copyright (c) 2019-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package terraform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattermost/mattermost-load-test-ng/deployment/terraform/ssh"

	"github.com/mattermost/mattermost-server/v6/shared/mlog"
)

const (
	defaultGrafanaUsernamePass = "admin:admin"
	defaultRequestTimeout      = 10 * time.Second
)

func doAPIRequest(url, method string, payload io.Reader) (string, error) {
	// Set preference to new dashboard.
	ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, url, payload)
	if err != nil {
		return "", err
	}
	req.Header.Add("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Dump body.
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bad response: %s", string(data))
	}

	return string(data), nil
}

// UploadDashboard uploads the given dashboard to Grafana and returns its URL.
// Returns an error in case of failure.
func (t *Terraform) UploadDashboard(dashboard string) (string, error) {
	output, err := t.Output()
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("http://%s@%s:3000/api/dashboards/db", defaultGrafanaUsernamePass, output.MetricsServer.PublicIP)
	data := fmt.Sprintf(`{"dashboard":%s,"folderId":0,"overwrite":true}`, dashboard)
	data, err = doAPIRequest(url, http.MethodPost, strings.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("Grafana API request failed: %w", err)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(data), &resp); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}
	url, ok := resp["url"].(string)
	if !ok {
		return "", fmt.Errorf("bad response, missing url")
	}

	return url, nil
}

func (t *Terraform) setupMetrics(extAgent *ssh.ExtAgent) error {
	// Updating Prometheus config
	sshc, err := extAgent.NewClient(t.output.MetricsServer.PublicIP)
	if err != nil {
		return err
	}

	var hosts string
	var mmTargets, nodeTargets, esTargets, ltTargets []string
	for i, val := range t.output.Instances {
		host := fmt.Sprintf("app-%d", i)
		mmTargets = append(mmTargets, fmt.Sprintf("'%s:8067'", host))
		nodeTargets = append(nodeTargets, fmt.Sprintf("'%s:9100'", host))
		hosts += fmt.Sprintf("%s %s\n", val.PrivateIP, host)
	}
	for i, val := range t.output.Agents {
		host := fmt.Sprintf("agent-%d", i)
		nodeTargets = append(nodeTargets, fmt.Sprintf("'%s:9100'", host))
		ltTargets = append(ltTargets, fmt.Sprintf("'%s:4000'", host))
		hosts += fmt.Sprintf("%s %s\n", val.PrivateIP, host)
	}
	if t.output.HasProxy() {
		host := "proxy"
		nodeTargets = append(nodeTargets, fmt.Sprintf("'%s:9100'", host))
		hosts += fmt.Sprintf("%s %s\n", t.output.Proxy.PrivateIP, host)
	}

	if t.output.HasElasticSearch() {
		esEndpoint := fmt.Sprintf("https://%s", t.output.ElasticSearchServer.Endpoint)
		esTargets = append(esTargets, "'metrics:9114'")

		mlog.Info("Enabling Elasticsearch exporter", mlog.String("host", t.output.MetricsServer.PublicIP))
		esExporterService := fmt.Sprintf(esExporterServiceFile, esEndpoint)
		rdr := strings.NewReader(esExporterService)
		if out, err := sshc.Upload(rdr, "/lib/systemd/system/es-exporter.service", true); err != nil {
			return fmt.Errorf("error upload elasticsearch exporter service file: output: %s, error: %w", out, err)
		}
		cmd := "sudo systemctl enable es-exporter"
		if out, err := sshc.RunCommand(cmd); err != nil {
			return fmt.Errorf("error running ssh command: cmd: %s, output: %s, err: %v", cmd, out, err)
		}

		mlog.Info("Starting Elasticsearch exporter", mlog.String("host", t.output.MetricsServer.PublicIP))
		cmd = "sudo service es-exporter restart"
		if out, err := sshc.RunCommand(cmd); err != nil {
			return fmt.Errorf("error running ssh command: cmd: %s, output: %s, err: %v", cmd, out, err)
		}
	}

	mlog.Info("Updating Prometheus config", mlog.String("host", t.output.MetricsServer.PublicIP))
	prometheusConfigFile := fmt.Sprintf(prometheusConfig,
		strings.Join(nodeTargets, ","),
		strings.Join(mmTargets, ","),
		strings.Join(esTargets, ","),
		strings.Join(ltTargets, ","),
	)
	rdr := strings.NewReader(prometheusConfigFile)
	if out, err := sshc.Upload(rdr, "/etc/prometheus/prometheus.yml", true); err != nil {
		return fmt.Errorf("error upload prometheus config: output: %s, error: %w", out, err)
	}
	metricsHostsFile := fmt.Sprintf(metricsHosts, hosts)
	rdr = strings.NewReader(metricsHostsFile)
	if out, err := sshc.Upload(rdr, "/etc/hosts", true); err != nil {
		return fmt.Errorf("error upload metrics hosts file: output: %s, error: %w", out, err)
	}

	mlog.Info("Starting Prometheus", mlog.String("host", t.output.MetricsServer.PublicIP))
	cmd := "sudo service prometheus restart"
	if out, err := sshc.RunCommand(cmd); err != nil {
		return fmt.Errorf("error running ssh command: cmd: %s, output: %s, err: %v", cmd, out, err)
	}

	mlog.Info("Setting up Grafana", mlog.String("host", t.output.MetricsServer.PublicIP))

	// Upload config file
	rdr = strings.NewReader(grafanaConfigFile)
	if out, err := sshc.Upload(rdr, "/etc/grafana/grafana.ini", true); err != nil {
		return fmt.Errorf("error upload grafana config: output: %s, error: %w", out, err)
	}

	// Upload datasource file
	buf, err := os.ReadFile(filepath.Join(t.dir, "datasource.yaml"))
	if err != nil {
		return err
	}
	dataSource := fmt.Sprintf(string(buf), "http://"+t.output.MetricsServer.PrivateIP+":9090")
	if out, err := sshc.Upload(strings.NewReader(dataSource), "/etc/grafana/provisioning/datasources/datasource.yaml", true); err != nil {
		return fmt.Errorf("error while uploading datasource: output: %s, error: %w", out, err)
	}

	// Upload dashboard file
	buf, err = os.ReadFile(filepath.Join(t.dir, "dashboard.yaml"))
	if err != nil {
		return err
	}
	if out, err := sshc.Upload(bytes.NewReader(buf), "/etc/grafana/provisioning/dashboards/dashboard.yaml", true); err != nil {
		return fmt.Errorf("error while uploading dashboard: output: %s, error: %w", out, err)
	}

	// Upload dashboard json
	buf, err = os.ReadFile(filepath.Join(t.dir, "dashboard_data.json"))
	if err != nil {
		return err
	}
	cmd = "sudo mkdir -p /var/lib/grafana/dashboards"
	if out, err := sshc.RunCommand(cmd); err != nil {
		return fmt.Errorf("error running ssh command: cmd: %s, output: %s, err: %v", cmd, out, err)
	}
	if out, err := sshc.Upload(bytes.NewReader(buf), "/var/lib/grafana/dashboards/dashboard.json", true); err != nil {
		return fmt.Errorf("error while uploading dashboard_json: output: %s, error: %w", out, err)
	}

	if t.output.HasElasticSearch() {
		buf, err = os.ReadFile(filepath.Join(t.dir, "es_dashboard_data.json"))
		if err != nil {
			return err
		}
		if out, err := sshc.Upload(bytes.NewReader(buf), "/var/lib/grafana/dashboards/es_dashboard.json", true); err != nil {
			return fmt.Errorf("error while uploading es_dashboard_json: output: %s, error: %w", out, err)
		}
	}

	// Restart grafana
	cmd = "sudo service grafana-server restart"
	if out, err := sshc.RunCommand(cmd); err != nil {
		return fmt.Errorf("error running ssh command: cmd: %s, output: %s, err: %v", cmd, out, err)
	}

	// Waiting for Grafana to be back up.
	url := fmt.Sprintf("http://%s@%s:3000/api/user/preferences", defaultGrafanaUsernamePass, t.output.MetricsServer.PublicIP)
	timeout := time.After(10 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			break
		}
		mlog.Info("Server not up yet, waiting...")
		select {
		case <-timeout:
			return errors.New("timeout: server is not responding")
		case <-time.After(1 * time.Second):
		}
	}

	payload := struct {
		Theme           string `json:"theme"`
		HomeDashboardID int    `json:"homeDashboardId"`
		Timezone        string `json:"timezone"`
	}{
		HomeDashboardID: 2,
	}
	data, err := json.Marshal(&payload)
	if err != nil {
		return err
	}

	resp, err := doAPIRequest(url, http.MethodPut, bytes.NewReader(data))
	if err != nil {
		return err
	}
	mlog.Info("Response: " + resp)

	return nil
}
