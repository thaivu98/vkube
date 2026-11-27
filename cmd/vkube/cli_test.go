package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func executeCmd(args ...string) (string, error) {
	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

func TestVersionCmd(t *testing.T) {
	out, err := executeCmd("version")
	if err != nil {
		t.Fatalf("version failed: %v", err)
	}
	if !strings.Contains(out, "BOM Release Tag:") || !strings.Contains(out, "kubernetes:") {
		t.Fatalf("unexpected version output:\n%s", out)
	}
}

func TestBundleExportAndDryRun(t *testing.T) {
	manifestFile := filepath.Join(t.TempDir(), "versions.yaml")
	if _, err := executeCmd("bundle", "export-manifest", "-o", manifestFile); err != nil {
		t.Fatalf("export-manifest failed: %v", err)
	}

	// Sửa thử 1 dòng version trong file vừa xuất và chạy dry-run với --manifest
	raw, err := os.ReadFile(manifestFile)
	if err != nil {
		t.Fatalf("read exported manifest: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "kubernetes:") {
			line = "  kubernetes: v1.36.9"
		}
		lines = append(lines, line)
	}
	modified := strings.Join(lines, "\n")
	if err := os.WriteFile(manifestFile, []byte(modified), 0o600); err != nil {
		t.Fatalf("write modified manifest: %v", err)
	}

	out, err := executeCmd("bundle", "build", "--arch", "arm64", "--manifest", manifestFile, "--dry-run")
	if err != nil {
		t.Fatalf("bundle build --dry-run failed: %v", err)
	}
	if !strings.Contains(out, "v1.36.9/bin/linux/arm64/kubeadm") {
		t.Fatalf("expected overridden arm64 kubeadm URL in dry-run output:\n%s", out)
	}
}

func TestInitConfigAndApplyDryRun(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "cluster.yaml")
	if _, err := executeCmd("init-config", "--arch", "amd64", "--ha", "-o", cfgPath); err != nil {
		t.Fatalf("init-config failed: %v", err)
	}

	out, err := executeCmd("apply", "-f", cfgPath, "--dry-run")
	if err != nil {
		t.Fatalf("apply --dry-run failed: %v", err)
	}
	if !strings.Contains(out, "ControlPlane:  3 node(s)") || !strings.Contains(out, "Workers:       2 node(s)") {
		t.Fatalf("unexpected apply --dry-run output:\n%s", out)
	}
}

func TestDay2SubcommandsRegistered(t *testing.T) {
	out, err := executeCmd("--help")
	if err != nil {
		t.Fatalf("root --help failed: %v", err)
	}
	for _, cmdName := range []string{"add-node", "remove-node", "backup", "restore", "certs", "addons"} {
		if !strings.Contains(out, cmdName) {
			t.Errorf("expected subcommand %q in root help output:\n%s", cmdName, out)
		}
	}
}
