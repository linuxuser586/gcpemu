package hosttest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Kube reads a cluster's Kubernetes API for failure diagnostics. It never
// fails a test: Diagnose reports a failed read, and Logs omits what it could
// not read.
type Kube struct {
	Endpoint string
	Client   *http.Client
}

// podSet is pods of a namespace, chosen by a pod list query parameter.
type podSet struct{ ns, param, selector string }

func (s podSet) path() string {
	return "/api/v1/namespaces/" + s.ns + "/pods?" + s.param + "=" + url.QueryEscape(s.selector)
}

func labelled(ns, selector string) podSet { return podSet{ns, "labelSelector", selector} }

func (k Kube) get(path string, out any) (int, []byte) {
	resp, err := k.Client.Get(k.Endpoint + path)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.Unmarshal(b, out); err != nil {
			return 0, fmt.Appendf(nil, "undecodable %d: %s", resp.StatusCode, b)
		}
	}
	return resp.StatusCode, b
}

// Logs returns the last lines of the pods matching selector.
func (k Kube) Logs(ns, selector string) string { return k.logs(labelled(ns, selector)) }

func (k Kube) logs(s podSet) string {
	var l struct {
		Items []struct{ Metadata struct{ Name string } }
	}
	k.get(s.path(), &l)
	var out strings.Builder
	for _, p := range l.Items {
		code, b := k.get("/api/v1/namespaces/"+s.ns+"/pods/"+p.Metadata.Name+"/log?tailLines=40", nil)
		if code != http.StatusOK {
			b = fmt.Appendf(nil, "log: %d %s", code, b)
		}
		fmt.Fprintf(&out, "--- %s\n%s\n", p.Metadata.Name, b)
	}
	return out.String()
}

// Diagnose returns what explains pods matching selector not being ready:
// their status, the nodes, the namespace's recent events and the logs. Logs
// alone say nothing of a pod that never ran (#966).
func (k Kube) Diagnose(ns, selector string) string { return k.diagnose(labelled(ns, selector)) }

// DiagnosePod is Diagnose for one pod by name, for a pod without labels to
// select.
func (k Kube) DiagnosePod(ns, name string) string {
	return k.diagnose(podSet{ns, "fieldSelector", "metadata.name=" + name})
}

func (k Kube) diagnose(s podSet) string {
	return k.podStatus(s) + k.nodeStatus() + k.events(s.ns) + k.logs(s)
}

// condition is a pod or node condition.
type condition struct{ Type, Status, Reason, Message string }

func (c condition) String() string {
	return strings.TrimSpace(c.Type + "=" + c.Status + " " + reasoned(c.Reason, c.Message))
}

// reasoned joins an API object's reason and message, either of which may be
// empty.
func reasoned(reason, message string) string {
	if reason == "" || message == "" {
		return reason + message
	}
	return reason + ": " + message
}

// containerState is a container's state, its one key being waiting, running
// or terminated.
type containerState map[string]struct {
	Reason, Message string
	ExitCode        int
}

func (s containerState) String() string {
	var out []string
	for kind, v := range s {
		line := strings.TrimSpace(kind + " " + reasoned(v.Reason, v.Message))
		if kind == "terminated" {
			line += fmt.Sprintf(" exit %d", v.ExitCode)
		}
		out = append(out, line)
	}
	return strings.Join(out, " ")
}

// containerStatus is a container's entry in a pod's status.
type containerStatus struct {
	Name             string
	RestartCount     int
	State, LastState containerState
}

func (c containerStatus) String() string {
	line := fmt.Sprintf("%s restarts=%d %s", c.Name, c.RestartCount, c.State)
	if len(c.LastState) > 0 {
		line += fmt.Sprintf(", last %s", c.LastState)
	}
	return line
}

// podStatus returns each matching pod's phase, node, unmet conditions and
// container states.
func (k Kube) podStatus(s podSet) string {
	var l struct {
		Items []struct {
			Metadata struct{ Name string }
			Spec     struct{ NodeName string }
			Status   struct {
				Phase, Reason, Message                   string
				Conditions                               []condition
				InitContainerStatuses, ContainerStatuses []containerStatus
			}
		}
	}
	if code, b := k.get(s.path(), &l); code != http.StatusOK {
		return fmt.Sprintf("pods: %d %s\n", code, b)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "pods %s %s:\n", s.ns, s.selector)
	for _, p := range l.Items {
		line := fmt.Sprintf("%s phase=%s node=%s %s", p.Metadata.Name, p.Status.Phase, p.Spec.NodeName, reasoned(p.Status.Reason, p.Status.Message))
		fmt.Fprintf(&out, "  %s\n", strings.TrimSpace(line))
		for _, c := range p.Status.Conditions {
			if c.Status != "True" {
				fmt.Fprintf(&out, "    %s\n", c)
			}
		}
		for _, c := range slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses) {
			fmt.Fprintf(&out, "    %s\n", c)
		}
	}
	return out.String()
}

// nodeStatus returns each node's readiness, its DiskPressure either way, its
// other conditions that are true, and its taints. DiskPressure always prints
// so a healthy node differs from one that did not report it (#993).
func (k Kube) nodeStatus() string {
	var l struct {
		Items []struct {
			Metadata struct{ Name string }
			Spec     struct {
				Taints []struct{ Key, Effect string }
			}
			Status struct{ Conditions []condition }
		}
	}
	if code, b := k.get("/api/v1/nodes", &l); code != http.StatusOK {
		return fmt.Sprintf("nodes: %d %s\n", code, b)
	}
	var out strings.Builder
	out.WriteString("nodes:\n")
	for _, n := range l.Items {
		line := []string{n.Metadata.Name}
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" || c.Type == "DiskPressure" || c.Status != "False" {
				line = append(line, c.String())
			}
		}
		var taints []string
		for _, t := range n.Spec.Taints {
			taints = append(taints, t.Key+":"+t.Effect)
		}
		if len(taints) > 0 {
			line = append(line, "taints=["+strings.Join(taints, " ")+"]")
		}
		fmt.Fprintf(&out, "  %s\n", strings.Join(line, " "))
	}
	return out.String()
}

// events returns the namespace's last 30 events, oldest first.
func (k Kube) events(ns string) string {
	type event struct {
		Type, Reason, Message    string
		Count                    int
		LastTimestamp, EventTime time.Time
		InvolvedObject           struct{ Kind, Name string }
	}
	var l struct{ Items []event }
	if code, b := k.get("/api/v1/namespaces/"+ns+"/events", &l); code != http.StatusOK {
		return fmt.Sprintf("events: %d %s\n", code, b)
	}
	// An event carries one of the two times, by the API that recorded it.
	at := func(e event) time.Time {
		if e.LastTimestamp.IsZero() {
			return e.EventTime
		}
		return e.LastTimestamp
	}
	slices.SortFunc(l.Items, func(a, b event) int { return at(a).Compare(at(b)) })
	var out strings.Builder
	fmt.Fprintf(&out, "events %s:\n", ns)
	for _, e := range l.Items[max(0, len(l.Items)-30):] {
		count := ""
		if e.Count > 1 {
			count = fmt.Sprintf(" x%d", e.Count)
		}
		fmt.Fprintf(&out, "  %s %s %s %s/%s%s: %s\n", at(e).UTC().Format(time.TimeOnly), e.Type, e.Reason, e.InvolvedObject.Kind, e.InvolvedObject.Name, count, e.Message)
	}
	return out.String()
}
