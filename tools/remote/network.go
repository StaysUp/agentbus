package main

// -network: after the checks, the runner's network control. The server's
// policy turns it on ([network] contain = "on") and allows one host
// ([network] allow, -allow), and the stand-in, asked
// "network probe", tries every way out from inside its turn: its gateway,
// the allowed host, a host not on the list, a raw IP (straight and through
// the proxy) and a DNS lookup. It writes what happened to a file in the
// runner's home, which the tool reads from the VM. Then the tool prints the
// network.refused rows of the server's record for that turn. On a runner
// that cannot close an agent off, the turn is refused, and the tool prints
// the reason.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// probeScript is what the stand-in runs for "network probe": one line per
// try, "<name> <http code>/<proxy's CONNECT code>"; 000 is no answer.
const probeScript = `r="$HOME/net-probe.txt"; : > "$r"
w() { curl -s -o /dev/null -m 10 -w '%{http_code}/%{http_connect}' "$@" 2>/dev/null; }
echo "gateway $(w -X POST -H 'content-type: application/json' -d '{}' "$ANTHROPIC_BASE_URL/v1/messages")" >> "$r"
echo "allowed $(w "https://$PROBE_ALLOWED/")" >> "$r"
echo "not-listed $(w https://example.com/)" >> "$r"
echo "raw-ip-via-proxy $(w -k https://1.1.1.1/)" >> "$r"
echo "raw-ip-straight $(w -k --noproxy '*' https://1.1.1.1/)" >> "$r"
if getent hosts example.com > /dev/null 2>&1; then echo "dns yes" >> "$r"; else echo "dns no" >> "$r"; fi
echo "proxy $HTTPS_PROXY" >> "$r"
`

func (w *world) network() error {
	fmt.Printf("\nnetwork control: [network] contain = \"on\", allow = [%q] on the server; the stand-in probes from its turn\n", w.o.allow)
	var p map[string]any
	if code, out := w.api.call("GET", "/api/v0/settings/permissions", nil, &p); code != 200 {
		return fmt.Errorf("GET permissions: %d %s", code, out)
	}
	p["network"] = map[string]any{"contain": "on", "allow": []string{w.o.allow}}
	if code, out := w.api.callMatch("PUT", "/api/v0/settings/permissions", p, "*"); code != 200 {
		return fmt.Errorf("PUT permissions: %d %s", code, out)
	}
	out, _ := lima(w.o.vm, "grep -i 'network' "+w.o.vmDir+"/runner.out | tail -1")
	fmt.Printf("  the runner said at start: %s\n", strings.TrimSpace(out))
	var created started
	t0 := time.Now()
	if code, out := w.api.call("POST", "/api/v0/turns", map[string]any{"agent": "claude",
		"folder_ref": w.ref, "connector": w.connector, "isolate": true, "prompt": "network probe"}, &created); code/100 != 2 {
		return fmt.Errorf("POST /api/v0/turns: %d %s", code, out)
	}
	var j job
	if err := waitFor(2*time.Minute, "the probe turn", func() bool {
		j = job{}
		w.api.jobOfTurn(created.Turn, &j)
		return j.Ended != nil
	}); err != nil {
		return err
	}
	fmt.Printf("  job %s after %s\n", j.State, time.Since(t0).Round(time.Millisecond))
	if j.State != "finished" {
		fmt.Printf("  reason: %s\n  result: %s\n", j.Reason, clip(string(j.Result), 600))
		return nil
	}
	res, _ := lima(w.o.vm, "cat "+w.o.vmDir+"/home/net-probe.txt")
	for _, l := range strings.Split(strings.TrimSpace(res), "\n") {
		fmt.Printf("  %s\n", l)
	}
	var r struct {
		Events []json.RawMessage
	}
	if code, out := w.api.call("GET", "/api/v0/sessions/"+created.ID+"/events?turn="+j.Turn+"&limit=1000", nil, &r); code != 200 {
		return fmt.Errorf("the turn's events: %d %s", code, out)
	}
	fmt.Println("  network.refused rows in the server's record:")
	n := 0
	for _, e := range r.Events {
		if strings.Contains(string(e), `"network.refused"`) {
			n++
			fmt.Printf("    %s\n", e)
		}
	}
	if n == 0 {
		return fmt.Errorf("no network.refused row in the server's record for %s", j.Turn)
	}
	return nil
}
