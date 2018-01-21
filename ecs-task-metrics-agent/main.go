package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/mackerelio/golib/logging"
	"github.com/mackerelio/mackerel-agent/config"
	"net/http"
	"os"
	"strconv"
	"strings"
)

var logger = logging.GetLogger("aggregate.agent")

type metricsPlugins map[string]*config.MetricPlugin

func (plugins metricsPlugins) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-MACKEREL-AGENT-PLUGIN-META") == "1" {
		plugins.collectMetricsMeta(w)
	} else {
		plugins.collectMetrics(w)
	}
}

func (plugins metricsPlugins) collectMetrics(w http.ResponseWriter) {
	length := 0
	metrics := make([]string, 0, len(plugins))
	for _, p := range plugins {
		stdout, stderr, _, err := p.Command.Run()

		if stderr != "" {
			logger.Infof("command %s outputted to STDERR: %q", p.Command.CommandString(), stderr)
		}
		if err != nil {
			logger.Errorf("Failed to execute command %s (skip)", p.Command.CommandString())
		}
		length += len(stdout)
		metrics = append(metrics, stdout)
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Length", strconv.Itoa(length))
	for _, m := range metrics {
		w.Write([]byte(m))
	}
}

const metaHeader = "# mackerel-agent-plugin\n"

type meta struct {
	Graphs map[string]json.RawMessage `json:"graphs"`
}

func (plugins metricsPlugins) collectMetricsMeta(w http.ResponseWriter) {
	ret := meta{Graphs: map[string]json.RawMessage{}}
	for _, p := range plugins {
		stdout, stderr, _, err := p.Command.RunWithEnv([]string{"MACKEREL_AGENT_PLUGIN_META=1"})

		if stderr != "" {
			logger.Infof("command %s outputted to STDERR: %q", p.Command.CommandString(), stderr)
		}
		if err != nil {
			logger.Warningf("Failed to execute command %s (skip)", p.Command.CommandString())
			continue
		}
		if !strings.HasPrefix(stdout, metaHeader) {
			logger.Warningf("Command %s generate invalid meta header. (skip)", p.Command.CommandString())
			continue
		}
		m := meta{}
		err = json.Unmarshal([]byte(strings.TrimLeft(stdout, metaHeader)), &m)
		if err != nil {
			logger.Warningf("Command %s generate invalid meta json. (skip)", p.Command.CommandString())
			continue
		}
		for k, v := range m.Graphs {
			ret.Graphs[k] = v
		}
	}
	buf, err := json.Marshal(&ret)
	if err != nil {
		logger.Errorf("Fail to generate meta json", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	w.Header().Set("X-MACKEREL-AGENT-PLUGIN-META", "1")
	w.Write(buf)
}

func main() {
	optPort := flag.Int("port", 2018, "Port")
	optConf := flag.String("conf", "", "Config file path")
	flag.Parse()

	conf, err := config.LoadConfig(*optConf)
	if err != nil {
		panic(err)
		os.Exit(99)
	}
	http.ListenAndServe(fmt.Sprintf(":%d", *optPort), metricsPlugins(conf.MetricPlugins))
}
