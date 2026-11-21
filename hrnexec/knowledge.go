package hrnexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	if err := ctx.Err(); err != nil {
		return packet, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	f, err := os.CreateTemp("", "ada-knowledge-*.json")
	if err != nil {
		return packet, fmt.Errorf("create knowledge request: %w", err)
	}
	defer os.Remove(f.Name())
	if err := errors.Join(json.NewEncoder(f).Encode(req), f.Close()); err != nil {
		return packet, fmt.Errorf("write knowledge request: %w", err)
	}
	cmd, err := hrnCommand(opts, "knowledge", "search", f.Name())
	if err != nil {
		return packet, err
	}
	stdout := boundedOutput{limit: 1 << 20}
	stderr := boundedOutput{limit: 512}
	// Redact the values actually inherited by this child even if the parent environment changes.
	cmd.Env = cmd.Environ()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return packet, fmt.Errorf("start hrn knowledge: %w", err)
	}
	kill, closeTree, err := manageProcessTree(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return packet, err
	}
	defer closeTree()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-ctx.Done():
		kill()
		<-done
		return packet, fmt.Errorf("hrn knowledge: %w", ctx.Err())
	case err := <-done:
		if err != nil {
			return packet, fmt.Errorf("hrn knowledge: %w: %s", err, stderr.diagnostic(cmd.Env))
		}
	}
	if stdout.overflow {
		return packet, fmt.Errorf("hrn knowledge output exceeds 1 MiB")
	}
	var response struct {
		*KnowledgePacket
		Packet json.RawMessage `json:"packet"`
	}
	if err := json.Unmarshal(stdout.buf.Bytes(), &response); err != nil {
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
