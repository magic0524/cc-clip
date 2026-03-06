package doctor

import (
	"fmt"
	"strings"

	"github.com/shunmei/cc-clip/internal/shim"
	"github.com/shunmei/cc-clip/internal/token"
)

func RunRemote(host string, port int) []CheckResult {
	var results []CheckResult

	// Check SSH connectivity
	out, err := shim.RemoteExec(host, "echo ok")
	if err != nil {
		results = append(results, CheckResult{"ssh", false, fmt.Sprintf("cannot connect to %s: %v", host, err)})
		return results
	}
	if strings.TrimSpace(out) != "ok" {
		results = append(results, CheckResult{"ssh", false, fmt.Sprintf("unexpected output: %s", out)})
		return results
	}
	results = append(results, CheckResult{"ssh", true, fmt.Sprintf("connected to %s", host)})

	// Check remote binary
	out, err = shim.RemoteExec(host, "~/.local/bin/cc-clip version")
	if err != nil {
		results = append(results, CheckResult{"remote-bin", false, "cc-clip not found at ~/.local/bin/cc-clip"})
	} else {
		results = append(results, CheckResult{"remote-bin", true, strings.TrimSpace(out)})
	}

	// Check shim installation — detect which target (xclip or wl-paste)
	shimTarget := ""
	for _, target := range []string{"xclip", "wl-paste"} {
		out, err = shim.RemoteExec(host, fmt.Sprintf("head -2 ~/.local/bin/%s 2>/dev/null || echo 'not found'", target))
		if err == nil && strings.Contains(out, "cc-clip") {
			shimTarget = target
			break
		}
	}
	if shimTarget != "" {
		results = append(results, CheckResult{"shim", true, fmt.Sprintf("%s shim installed", shimTarget)})
	} else {
		results = append(results, CheckResult{"shim", false, "no cc-clip shim found (checked xclip and wl-paste)"})
	}

	// Check PATH priority for the detected shim target
	checkTarget := "xclip"
	if shimTarget != "" {
		checkTarget = shimTarget
	}
	out, err = shim.RemoteExec(host, fmt.Sprintf("which %s 2>/dev/null || echo 'not in PATH'", checkTarget))
	if err == nil && strings.Contains(out, ".local/bin") {
		results = append(results, CheckResult{"path-order", true, fmt.Sprintf("%s resolves to %s", checkTarget, strings.TrimSpace(out))})
	} else {
		results = append(results, CheckResult{"path-order", false, fmt.Sprintf("%s resolves to %s (shim not first)", checkTarget, strings.TrimSpace(out))})
	}

	// Check tunnel from remote side
	out, err = shim.RemoteExec(host, fmt.Sprintf(
		"bash -c 'echo >/dev/tcp/127.0.0.1/%d' 2>&1 && echo 'tunnel ok' || echo 'tunnel fail'", port))
	if strings.Contains(out, "tunnel ok") {
		results = append(results, CheckResult{"tunnel", true, fmt.Sprintf("port %d forwarded", port)})
	} else {
		results = append(results, CheckResult{"tunnel", false, fmt.Sprintf("port %d not reachable from remote", port)})
	}

	// Check token on remote
	out, err = shim.RemoteExec(host, "test -f ~/.cache/cc-clip/session.token && echo 'present' || echo 'missing'")
	if strings.Contains(out, "present") {
		results = append(results, CheckResult{"remote-token", true, "token file present"})
	} else {
		results = append(results, CheckResult{"remote-token", false, "token file missing"})
	}

	// Check remote token matches local token
	results = append(results, checkTokenMatch(host)...)

	// Check deploy state file
	results = append(results, checkDeployState(host)...)

	// Check PATH fix (rc file marker)
	results = append(results, checkPathFix(host)...)

	// End-to-end image round-trip (only if tunnel is up)
	if tunnelOK(results) {
		results = append(results, runImageProbe(host, port)...)
	}

	return results
}

func tunnelOK(results []CheckResult) bool {
	for _, r := range results {
		if r.Name == "tunnel" && r.OK {
			return true
		}
	}
	return false
}

func runImageProbe(host string, port int) []CheckResult {
	// Run the probe FROM the remote host through the tunnel, not from local.
	// This validates the full chain: remote -> tunnel -> daemon.
	cmd := fmt.Sprintf(
		`TOKEN=$(cat ~/.cache/cc-clip/session.token 2>/dev/null) && `+
			`curl -sf --max-time 5 `+
			`-H "Authorization: Bearer ${TOKEN}" `+
			`-H "User-Agent: cc-clip/0.1" `+
			`"http://127.0.0.1:%d/clipboard/type"`,
		port)

	out, err := shim.RemoteExec(host, cmd)
	if err != nil {
		return []CheckResult{{"image-probe", false, fmt.Sprintf("remote probe failed: %v (%s)", err, strings.TrimSpace(out))}}
	}

	out = strings.TrimSpace(out)
	if strings.Contains(out, `"type":"image"`) {
		return []CheckResult{{"image-probe", true, "clipboard has image (verified from remote)"}}
	}
	if strings.Contains(out, `"type":`) {
		return []CheckResult{{"image-probe", true, fmt.Sprintf("remote response: %s (copy an image to test)", out)}}
	}
	return []CheckResult{{"image-probe", false, fmt.Sprintf("unexpected response: %s", out)}}
}

// checkTokenMatch verifies the remote token matches the local daemon token.
func checkTokenMatch(host string) []CheckResult {
	localToken, err := token.ReadTokenFile()
	if err != nil {
		return []CheckResult{{"token-match", false, "cannot read local token to compare"}}
	}

	remoteToken, err := shim.RemoteExec(host, "cat ~/.cache/cc-clip/session.token 2>/dev/null")
	if err != nil || strings.TrimSpace(remoteToken) == "" {
		return []CheckResult{{"token-match", false, "cannot read remote token"}}
	}

	if strings.TrimSpace(remoteToken) == localToken {
		return []CheckResult{{"token-match", true, "remote token matches local"}}
	}
	return []CheckResult{{"token-match", false, "remote token differs from local (re-run 'cc-clip connect')"}}
}

// checkDeployState checks if the deploy state file exists on the remote.
func checkDeployState(host string) []CheckResult {
	out, err := shim.RemoteExec(host, "cat ~/.cache/cc-clip/deploy.json 2>/dev/null || echo 'not found'")
	if err != nil || strings.Contains(out, "not found") || strings.TrimSpace(out) == "" {
		return []CheckResult{{"deploy-state", false, "deploy.json not found (deploy state not tracked)"}}
	}

	// Basic validation: check it contains expected fields
	if strings.Contains(out, "binary_hash") {
		return []CheckResult{{"deploy-state", true, "deploy.json present and valid"}}
	}
	return []CheckResult{{"deploy-state", false, "deploy.json exists but may be malformed"}}
}

// checkPathFix verifies the PATH marker block exists in the remote shell rc file.
func checkPathFix(host string) []CheckResult {
	fixed, err := shim.IsPathFixed(host)
	if err != nil {
		return []CheckResult{{"path-fix", false, fmt.Sprintf("cannot check PATH marker: %v", err)}}
	}
	if fixed {
		return []CheckResult{{"path-fix", true, "PATH marker present in shell rc file"}}
	}
	return []CheckResult{{"path-fix", false, "PATH marker not found in shell rc file"}}
}
