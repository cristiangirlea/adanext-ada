package hrnexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type KnowledgeRequest struct {
	TenantID  string `json:"tenant_id"`
	ProjectID string `json:"project_id"`
	Backend   string `json:"backend"`
	Model     string `json:"model"`
	Revision  string `json:"revision"`
	Query     string `json:"query"`
}

type KnowledgeEntry struct {
	Key     string `json:"key"`
	Content string `json:"content"`
	Source  string `json:"source"`
	Excerpt bool   `json:"excerpt"`
}

type KnowledgePacket struct {
	VersionID   string           `json:"version_id"`
	AuditID     string           `json:"audit_id"`
	EmptyReason string           `json:"empty_reason"`
	Entries     []KnowledgeEntry `json:"entries"`
}

type boundedOutput struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := min(len(p), b.limit-b.buf.Len())
	b.buf.Write(p[:n])
	if n < len(p) {
		b.overflow = true
	}
	return len(p), nil
}

func (b *boundedOutput) diagnostic(env []string) string {
	text := b.buf.String()
	for _, entry := range env {
		key, secret, _ := strings.Cut(entry, "=")
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		if (key != "HRN_TENANT_TOKEN" && key != "HRN_SERVER_URL") || secret == "" {
			continue
		}
		text = strings.ReplaceAll(text, secret, "[redacted]")
		// A credential crossing the capture boundary must also be removed.
		if b.overflow {
			for size := min(len(text), len(secret)-1); size > 0; size-- {
				if strings.HasSuffix(text, secret[:size]) {
					text = text[:len(text)-size]
					break
				}
			}
		}
	}
	return strings.TrimSpace(text[:min(len(text), b.limit)])
}

func SearchKnowledge(ctx context.Context, opts Options, req KnowledgeRequest) (KnowledgePacket, error) {
	var packet KnowledgePacket
	data, err := runKnowledge(ctx, opts, 30*time.Second, req, func(path string) []string { return []string{"search", path} })
	if err != nil {
		return packet, err
	}
	var response struct {
		*KnowledgePacket
		Packet json.RawMessage `json:"packet"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return packet, fmt.Errorf("decode knowledge response: %w", err)
	}
	if response.Packet != nil {
		var nested struct{ *KnowledgePacket }
		if err := json.Unmarshal(response.Packet, &nested); err != nil {
			return packet, fmt.Errorf("decode knowledge packet: %w", err)
		}
		response.KnowledgePacket = nested.KnowledgePacket
	}
	if response.KnowledgePacket == nil {
		return packet, fmt.Errorf("missing knowledge packet fields")
	}
	return *response.KnowledgePacket, nil
}

func runKnowledge(ctx context.Context, opts Options, timeout time.Duration, req any, args func(string) []string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	f, err := os.CreateTemp("", "ada-knowledge-*.json")
	if err != nil {
		return nil, fmt.Errorf("create knowledge request: %w", err)
	}
	defer os.Remove(f.Name())
	if err := errors.Join(json.NewEncoder(f).Encode(req), f.Close()); err != nil {
		return nil, fmt.Errorf("write knowledge request: %w", err)
	}
	cmd, err := hrnCommand(opts, append([]string{"knowledge"}, args(f.Name())...)...)
	if err != nil {
		return nil, err
	}
	stdout := boundedOutput{limit: 1 << 20}
	stderr := boundedOutput{limit: 512}
	// Redact the values actually inherited by this child even if the parent environment changes.
	cmd.Env = cmd.Environ()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start hrn knowledge: %w", err)
	}
	kill, closeTree, err := manageProcessTree(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	defer closeTree()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case <-ctx.Done():
		kill()
		<-done
		return nil, fmt.Errorf("hrn knowledge: %w", ctx.Err())
	case waitErr = <-done:
	}
	if waitErr != nil {
		waitErr = fmt.Errorf("hrn knowledge: %w: %s", waitErr, stderr.diagnostic(cmd.Env))
	}
	if stdout.overflow {
		return nil, fmt.Errorf("hrn knowledge output exceeds 1 MiB")
	}
	return stdout.buf.Bytes(), waitErr
}

type KnowledgeScope struct {
	TenantID  string `json:"tenant_id"`
	ProjectID string `json:"project_id"`
	Backend   string `json:"backend"`
	Model     string `json:"model"`
	Revision  string `json:"revision"`
}

type KnowledgeDraft struct {
	KnowledgeScope
	Origin         string           `json:"origin"`
	ReviewedBy     string           `json:"reviewed_by"`
	ReviewEvidence string           `json:"review_evidence"`
	Entries        []KnowledgeEntry `json:"entries"`
}

func ProposeKnowledge(ctx context.Context, opts Options, timeout time.Duration, scope KnowledgeScope, backend string, runs []string, output string) (KnowledgeProposal, error) {
	if backend != "claude" && backend != "codex" {
		return KnowledgeProposal{}, fmt.Errorf("knowledge propose requires claude or codex")
	}
	base := KnowledgeDraft{KnowledgeScope: scope, Entries: []KnowledgeEntry{}}
	data, err := runKnowledge(ctx, opts, timeout, base, func(path string) []string {
		args := []string{"propose", "-request", path, "-b", backend}
		for _, id := range runs {
			args = append(args, "-run", id)
		}
		return append(args, "-json", "-o", output)
	})
	partial := false
	if err != nil {
		exit, ok := errors.AsType[*exec.ExitError](err)
		if !ok || exit.ExitCode() != 3 {
			return KnowledgeProposal{}, err
		}
		partial = true
	}
	return parseKnowledgeProposal(data, runs, output, partial)
}

type KnowledgeVersion struct {
	KnowledgeScope
	ID        string `json:"id"`
	Admission struct {
		Verdict string `json:"verdict"`
	} `json:"admission"`
	Entries []KnowledgeEntry `json:"entries"`
}

func InspectKnowledge(ctx context.Context, opts Options, timeout time.Duration, scope KnowledgeScope, version string) (KnowledgeVersion, error) {
	req := struct {
		KnowledgeScope
		Version string `json:"version_id"`
	}{scope, version}
	var result KnowledgeVersion
	data, err := runKnowledge(ctx, opts, timeout, req, func(path string) []string { return []string{"inspect", path} })
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return result, fmt.Errorf("decode knowledge version: %w", err)
	}
	if result.ID != version || result.KnowledgeScope != scope {
		return result, fmt.Errorf("inspected version does not match the requested version and scope")
	}
	return result, nil
}
