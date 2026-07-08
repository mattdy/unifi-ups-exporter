// unifi-ups-exporter is a deliberately minimal Prometheus exporter for the
// embedded NUT server on Ubiquiti UniFi UPS devices.
//
// Unlike general-purpose NUT exporters, it speaks only the two commands the
// scrape actually needs -- LIST UPS and LIST VAR -- and never issues VER or
// NETVER. That is intentional: the UniFi NUT server returns a non-standard
// version banner and does not answer the usual handshake cleanly, which is
// exactly what desyncs go.nut-based exporters (DRuggeri, Telegraf) and trips
// HON95's version regex. By not asking, we sidestep the whole problem.
//
// It follows the multi-target exporter pattern: point Prometheus/Alloy at
// /metrics?target=<host> and fan out targets with relabelling, so a single
// instance scrapes many UPSs.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const namespace = "network_ups_tools"

var (
	listenAddr    = flag.String("web.listen-address", ":9199", "Address to listen on for HTTP requests.")
	metricsPath   = flag.String("web.telemetry-path", "/metrics", "Path under which to serve UPS metrics. Expects a ?target= query parameter.")
	defaultPort   = flag.String("nut.default-port", "3493", "NUT port to use when a target does not specify one.")
	scrapeTimeout = flag.Duration("nut.timeout", 5*time.Second, "Overall timeout for a single target scrape.")
)

// ---------------------------------------------------------------------------
// Minimal NUT client
// ---------------------------------------------------------------------------

type nutClient struct {
	conn    net.Conn
	r       *bufio.Reader
	timeout time.Duration
}

func dialNUT(target string, timeout time.Duration) (*nutClient, error) {
	if _, _, err := net.SplitHostPort(target); err != nil {
		// No port supplied; apply the default.
		target = net.JoinHostPort(target, *defaultPort)
	}
	conn, err := net.DialTimeout("tcp", target, timeout)
	if err != nil {
		return nil, err
	}
	return &nutClient{conn: conn, r: bufio.NewReader(conn), timeout: timeout}, nil
}

func (c *nutClient) close() { _ = c.conn.Close() }

// list issues "LIST <sub>" and returns the payload lines between the
// BEGIN/END markers. It reuses one buffered reader for the life of the
// connection, so no read-ahead bytes are ever discarded between commands.
func (c *nutClient) list(sub string) ([]string, error) {
	_ = c.conn.SetDeadline(time.Now().Add(c.timeout))
	if _, err := fmt.Fprintf(c.conn, "LIST %s\n", sub); err != nil {
		return nil, fmt.Errorf("writing LIST %s: %w", sub, err)
	}
	end := "END LIST " + sub
	var out []string
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("reading LIST %s: %w", sub, err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == end:
			return out, nil
		case strings.HasPrefix(line, "BEGIN LIST "):
			// skip the opening marker
		case strings.HasPrefix(line, "ERR "):
			return nil, fmt.Errorf("NUT returned error for LIST %s: %s", sub, strings.TrimPrefix(line, "ERR "))
		default:
			out = append(out, line)
		}
	}
}

// scrapeTarget connects, enumerates UPSs and reads every variable for each.
// The returned map is ups name -> (variable -> value).
func scrapeTarget(target string, timeout time.Duration) (map[string]map[string]string, error) {
	c, err := dialNUT(target, timeout)
	if err != nil {
		return nil, err
	}
	defer c.close()

	upsLines, err := c.list("UPS")
	if err != nil {
		return nil, err
	}

	result := make(map[string]map[string]string)
	for _, line := range upsLines {
		name, ok := parseUPSName(line)
		if !ok {
			continue
		}
		varLines, err := c.list("VAR " + name)
		if err != nil {
			return nil, err
		}
		vars := make(map[string]string, len(varLines))
		for _, vl := range varLines {
			if k, v, ok := parseVar(vl, name); ok {
				vars[k] = v
			}
		}
		result[name] = vars
	}
	return result, nil
}

// parseUPSName extracts "argon" from: UPS argon "UPS identifier"
func parseUPSName(line string) (string, bool) {
	const p = "UPS "
	if !strings.HasPrefix(line, p) {
		return "", false
	}
	rest := strings.TrimPrefix(line, p)
	if i := strings.IndexByte(rest, ' '); i >= 0 {
		rest = rest[:i]
	}
	return rest, rest != ""
}

// parseVar extracts key and value from: VAR argon battery.charge "100"
func parseVar(line, ups string) (key, val string, ok bool) {
	prefix := "VAR " + ups + " "
	if !strings.HasPrefix(line, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(line, prefix)
	i := strings.IndexByte(rest, ' ')
	if i < 0 {
		return "", "", false
	}
	return rest[:i], unquote(rest[i+1:]), true
}

// unquote strips surrounding quotes and unescapes \" and \\ per the NUT
// protocol's quoting rules.
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\') {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Prometheus text exposition
// ---------------------------------------------------------------------------

type family struct {
	help    string
	samples []string
}

type exposition struct {
	order []string
	fams  map[string]*family
}

func newExposition() *exposition {
	return &exposition{fams: make(map[string]*family)}
}

func (e *exposition) add(name, help string, value float64, labels ...[2]string) {
	f, ok := e.fams[name]
	if !ok {
		f = &family{help: help}
		e.fams[name] = f
		e.order = append(e.order, name)
	}
	var lb strings.Builder
	if len(labels) > 0 {
		lb.WriteByte('{')
		for i, kv := range labels {
			if i > 0 {
				lb.WriteByte(',')
			}
			lb.WriteString(kv[0])
			lb.WriteString(`="`)
			lb.WriteString(escapeLabel(kv[1]))
			lb.WriteByte('"')
		}
		lb.WriteByte('}')
	}
	f.samples = append(f.samples,
		name+lb.String()+" "+strconv.FormatFloat(value, 'g', -1, 64))
}

func (e *exposition) String() string {
	var sb strings.Builder
	for _, name := range e.order {
		f := e.fams[name]
		if f.help != "" {
			fmt.Fprintf(&sb, "# HELP %s %s\n", name, f.help)
		}
		fmt.Fprintf(&sb, "# TYPE %s gauge\n", name)
		for _, s := range f.samples {
			sb.WriteString(s)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func escapeLabel(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

// metricName turns a NUT variable such as "input.voltage.nominal" into
// "network_ups_tools_input_voltage_nominal".
func metricName(nutVar string) string {
	var b strings.Builder
	b.WriteString(namespace)
	b.WriteByte('_')
	for _, r := range nutVar {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// infoFields are string variables surfaced as labels on a *_device_info metric.
var infoFields = []struct{ nut, label string }{
	{"ups.mfr", "manufacturer"},
	{"ups.model", "model"},
	{"ups.serial", "serial"},
	{"ups.firmware", "firmware"},
	{"device.mfr", "device_manufacturer"},
	{"device.model", "device_model"},
}

// buildExposition converts scraped data into an exposition. Deterministic
// ordering (sorted names and keys) keeps output stable across scrapes.
func buildExposition(upses map[string]map[string]string, success float64, dur time.Duration) *exposition {
	exp := newExposition()

	names := make([]string, 0, len(upses))
	for n := range upses {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		vars := upses[name]

		keys := make([]string, 0, len(vars))
		for k := range vars {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, k := range keys {
			v := vars[k]
			if k == "ups.status" {
				for _, flag := range strings.Fields(v) {
					exp.add(namespace+"_ups_status",
						"UPS status flags from the NUT ups.status variable (1 per active flag).",
						1, [2]string{"ups", name}, [2]string{"flag", flag})
				}
				continue
			}
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				exp.add(metricName(k), "NUT variable "+k+".", f, [2]string{"ups", name})
			}
		}

		labels := [][2]string{{"ups", name}}
		haveInfo := false
		for _, fld := range infoFields {
			if v, ok := vars[fld.nut]; ok && v != "" {
				labels = append(labels, [2]string{fld.label, v})
				haveInfo = true
			}
		}
		if haveInfo {
			exp.add(namespace+"_device_info", "UPS device metadata (constant 1).", 1, labels...)
		}
	}

	exp.add(namespace+"_scrape_success",
		"1 if the NUT target was reachable and parsed successfully, 0 otherwise.", success)
	exp.add(namespace+"_scrape_duration_seconds",
		"Time taken to scrape the NUT target.", dur.Seconds())
	return exp
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if target == "" {
		http.Error(w, "missing required 'target' query parameter", http.StatusBadRequest)
		return
	}

	start := time.Now()
	upses, err := scrapeTarget(target, *scrapeTimeout)
	success := 1.0
	if err != nil {
		success = 0.0
		upses = nil
		log.Printf("scrape of %q failed: %v", target, err)
	}

	exp := buildExposition(upses, success, time.Since(start))
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(exp.String()))
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, `<html>
<head><title>UniFi UPS Exporter</title></head>
<body>
<h1>UniFi UPS Exporter</h1>
<p>Scrape a UPS: <a href="%s?target=10.10.30.1"><code>%s?target=&lt;host&gt;</code></a></p>
</body>
</html>`, *metricsPath, *metricsPath)
}

func main() {
	flag.Parse()

	http.HandleFunc(*metricsPath, handleMetrics)
	http.HandleFunc("/", handleRoot)

	log.Printf("unifi-ups-exporter listening on %s (scrape %s?target=<host>)", *listenAddr, *metricsPath)
	srv := &http.Server{
		Addr:              *listenAddr,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
