package server

import (
	"encoding/json"
	"testing"
)

func TestAlertmanagerLifecycleIdentityIncludesChannelAndFallback(t *testing.T) {
	firing := json.RawMessage(`[{"title":"[FIRING:1] HostCpuHighIowait for ","fallback":"[FIRING:1] HostCpuHighIowait units warning | https://example.test"}]`)
	resolved := json.RawMessage(`[{"title":"[RESOLVED] HostCpuHighIowait for ","fallback":"[RESOLVED] HostCpuHighIowait units warning | https://example.test"}]`)
	otherInstance := json.RawMessage(`[{"title":"[FIRING:1] HostCpuHighIowait for ","fallback":"[FIRING:1] HostCpuHighIowait mxs warning | https://example.test"}]`)

	firingState, firingKey := alertmanagerLifecycle("prom-mxs", firing)
	resolvedState, resolvedKey := alertmanagerLifecycle("#prom-mxs", resolved)
	_, otherChannelKey := alertmanagerLifecycle("prometheus", firing)
	_, otherInstanceKey := alertmanagerLifecycle("prom-mxs", otherInstance)
	if firingState != "firing" || resolvedState != "resolved" || firingKey == "" || firingKey != resolvedKey {
		t.Fatalf("lifecycle identities firing=(%q,%q) resolved=(%q,%q)", firingState, firingKey, resolvedState, resolvedKey)
	}
	if firingKey == otherChannelKey || firingKey == otherInstanceKey {
		t.Fatalf("identity collision: firing=%q channel=%q instance=%q", firingKey, otherChannelKey, otherInstanceKey)
	}
}
