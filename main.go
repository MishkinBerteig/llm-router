// llm-router is a single front door for OpenAI-compatible LLM backends.
//
// Clients only ever name a tier — "amazing", "great", "fast" — and the router
// decides which backend serves it and what that backend calls the model. The
// tier name is the stable interface; the backend behind it is an
// implementation detail that can move between LM Studio, Splash, rapid-mlx or
// another machine without any client knowing.
//
// Re-pointing a tier is an edit to the config file. It takes effect on the
// next request with no restart, and because the file is the only state, the
// change survives a restart by construction — there is no database to drift
// from the file and no second place to look.
//
// Standard library only, deliberately. A host that fronts local models usually
// also holds model-server tokens, agent credentials and SSH keys, so a
// dependency tree is a liability there rather than a convenience.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A Tier is one client-facing name and the backend that serves it.
type Tier struct {
	// Where the backend's OpenAI-compatible API lives, including the
	// version prefix, e.g. http://127.0.0.1:12345/v1
	BaseURL string `json:"base_url"`
	// What the BACKEND calls this model. The whole point of the router: a
	// client asks for "fast" and the backend may want "incoai/Qwen3.8-27B-Splash".
	Model string `json:"model"`
	// Credential for the backend. Prefer api_key_env so the token is not a
	// second copy sitting in this file.
	APIKey    string `json:"api_key,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// What the tier serves: "chat" (the default) for JSON endpoints such as
	// /v1/chat/completions, or "transcription" for POST /v1/audio/transcriptions.
	// Declared rather than guessed from the path so that a chat tier can never
	// be handed an audio upload, nor an audio tier a prompt.
	Kind string `json:"kind,omitempty"`
}

const (
	kindChat          = "chat"
	kindTranscription = "transcription"

	// The one audio endpoint a transcription tier serves.
	transcriptionPath = "/v1/audio/transcriptions"
)

// kind is the effective kind: an omitted one means chat.
func (t Tier) kind() string {
	if t.Kind == "" {
		return kindChat
	}
	return t.Kind
}

func (t Tier) transcription() bool { return t.kind() == kindTranscription }

// key resolves the backend credential, environment first.
func (t Tier) key() string {
	if t.APIKeyEnv != "" {
		return os.Getenv(t.APIKeyEnv)
	}
	return t.APIKey
}

type Config struct {
	// Address to serve on. Must be 0.0.0.0 to be reachable from Docker via
	// host.docker.internal, which is how Open WebUI gets here.
	Listen string          `json:"listen"`
	Tiers  map[string]Tier `json:"tiers"`

	// Front-door credentials. Set at least one whenever the router is
	// reachable from outside the host, e.g. published through a tunnel.
	//
	// It is a list because the fleet legitimately holds several: LM Studio
	// issues keys as sk-lm-<id>:<secret>, and this stack has a different one
	// per client. Keeping them distinct is worth it — a machine can be
	// revoked by deleting its entry here, without reissuing to everyone else.
	ClientAPIKey    string   `json:"client_api_key,omitempty"`
	ClientAPIKeyEnv string   `json:"client_api_key_env,omitempty"`
	ClientAPIKeys   []string `json:"client_api_keys,omitempty"`

	// Optional: let a backend decide whether an unrecognised key is genuine.
	//
	// LM Studio keeps its API keys somewhere this router cannot read — not in
	// ~/.lmstudio, not in its Application Support tree — so a key added in its
	// GUI can never be picked up by parsing a file. Instead the router asks:
	// one GET against the verifier with the presented bearer, and a 200 means
	// the key is real. A key created in the GUI therefore works here straight
	// away, and because this check runs before any tier is chosen, the key
	// requirement covers every backend — including Splash, which has no auth
	// of its own.
	Verifier *Verifier `json:"client_key_verifier,omitempty"`

	// Request auditing. This router is the single point every LLM call on the
	// host passes through, which makes it the only place a complete record
	// can be kept.
	Audit *Audit `json:"audit,omitempty"`

	// Who a credential belongs to, keyed by key ID (the non-secret part of
	// sk-lm-<id>:<secret>). Purely for making the audit log readable: a line
	// saying "alice" beats one saying "sk-lm-AbCdEfGh". Keys verified
	// dynamically by LM Studio appear by ID until labelled here.
	ClientLabels map[string]string `json:"client_labels,omitempty"`

	// Refuse a reply whose model is neither the tier nor the tier's backend
	// model. Defaults ON. A backend that answers as something else is serving
	// the wrong model, which is worse than an error. Set false to disable
	// without a rebuild — the config hot-reloads.
	VerifyResponseModel *bool `json:"verify_response_model,omitempty"`

	// Fail fast when nothing is listening, rather than leaving a caller to
	// discover it after its own timeout. An agent pointed at a dead backend
	// stalling for an hour is the failure this exists to prevent.
	DialTimeoutMS int `json:"dial_timeout_ms,omitempty"`
	// How long to wait for the first response byte. Generous by default:
	// prefill on a long prompt against a large model legitimately takes
	// minutes before anything comes back.
	ResponseHeaderTimeoutMS int `json:"response_header_timeout_ms,omitempty"`

	// Cap on a multipart request body, i.e. an audio upload. Applies to
	// multipart only: JSON bodies stay uncapped, because a long-context or
	// vision request is legitimately enormous. The default is a little over
	// OpenAI's own 25 MB limit for transcription uploads.
	MaxUploadBytes int `json:"max_upload_bytes,omitempty"`
}

// An Audit configures the request log. One JSON object per line, one file per
// day, pruned on a rolling window.
//
// JSON Lines rather than plain text on purpose: a prompt contains newlines and
// quotes, and an audit record that a prompt can break the format of is not an
// audit record. Every line stays greppable and `jq`-able.
type Audit struct {
	// Directory for audit-YYYY-MM-DD.jsonl. Created 0700; files 0600 —
	// with bodies on, this holds everything everyone typed.
	Dir string `json:"dir"`
	// Days to keep. Older files are deleted when the day rolls over.
	RetainDays int `json:"retain_days,omitempty"`
	// Record the request body, i.e. the prompt. On by default: it is the
	// part that shows what someone actually asked for.
	LogBodies *bool `json:"log_bodies,omitempty"`
	// Cap per record so one enormous context cannot dominate a day's file.
	MaxBodyBytes int `json:"max_body_bytes,omitempty"`
	// Record response bodies too. Off by default: for a stream this means
	// buffering the whole completion, and the prompt is what matters for
	// audit. Turn it on only if you need both halves of the conversation.
	LogResponses bool `json:"log_responses,omitempty"`
}

func (a *Audit) retain() int {
	if a.RetainDays > 0 {
		return a.RetainDays
	}
	return 7
}

func (a *Audit) bodies() bool {
	return a.LogBodies == nil || *a.LogBodies
}

func (a *Audit) maxBody() int {
	if a.MaxBodyBytes > 0 {
		return a.MaxBodyBytes
	}
	return 64 * 1024
}

// auditLog writes the records and owns rotation.
type auditLog struct {
	mu   sync.Mutex
	dir  string
	keep int
	day  string
	file *os.File
}

var audit *auditLog

func newAuditLog(a *Audit) (*auditLog, error) {
	if err := os.MkdirAll(a.Dir, 0o700); err != nil {
		return nil, err
	}
	return &auditLog{dir: a.Dir, keep: a.retain()}, nil
}

// rollover opens today's file, closing yesterday's and pruning anything past
// the window. Caller holds the lock.
func (l *auditLog) rollover(now time.Time) error {
	day := now.Format("2006-01-02")
	if l.file != nil && l.day == day {
		return nil
	}
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
	path := filepath.Join(l.dir, "audit-"+day+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	l.file, l.day = f, day
	l.prune(now)
	return nil
}

// prune deletes audit files older than the retention window. Driven by the
// date in the filename, so a clock change cannot strand a file forever.
func (l *auditLog) prune(now time.Time) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	cutoff := now.AddDate(0, 0, -l.keep)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "audit-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(name, "audit-"), ".jsonl")
		when, err := time.ParseInLocation("2006-01-02", stamp, now.Location())
		if err != nil {
			continue
		}
		if when.Before(cutoff) {
			if err := os.Remove(filepath.Join(l.dir, name)); err == nil {
				log.Printf("audit: pruned %s (older than %d days)", name, l.keep)
			}
		}
	}
}

func (l *auditLog) write(rec map[string]any) {
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.rollover(time.Now()); err != nil {
		log.Printf("audit: cannot open log: %v", err)
		return
	}
	l.file.Write(append(line, '\n'))
}

// keyID is the identifying, NON-SECRET part of a credential. LM Studio issues
// sk-lm-<id>:<secret>, so everything before the colon names the key without
// revealing it — and it matches what the GUI shows. Anything else is reduced
// to a short hash. The secret is never written anywhere.
func keyID(key string) string {
	if key == "" {
		return ""
	}
	if i := strings.Index(key, ":"); i > 0 {
		return key[:i]
	}
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

// record starts an audit record for a request. Returns nil when auditing is
// off, and every caller tolerates that.
func record(c *Config, r *http.Request) map[string]any {
	if audit == nil {
		return nil
	}
	id := keyID(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	rec := map[string]any{
		"ts":     time.Now().Format(time.RFC3339),
		"method": r.Method,
		"path":   r.URL.Path,
		// The direct peer. Behind a tunnel this is loopback for everyone,
		// which is exactly why forwarded_for is recorded beside it.
		"remote": r.RemoteAddr,
		"key":    id,
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		rec["forwarded_for"] = xff
	}
	if ua := r.Header.Get("User-Agent"); ua != "" {
		rec["user_agent"] = ua
	}
	if label, ok := c.ClientLabels[id]; ok {
		rec["label"] = label
	}
	return rec
}

// finish stamps the outcome and writes the record.
func finish(rec map[string]any, start time.Time, status int, outcome string) {
	if rec == nil {
		return
	}
	rec["status"] = status
	rec["outcome"] = outcome
	rec["ms"] = time.Since(start).Milliseconds()
	audit.write(rec)
}

// A Verifier is a backend trusted to say whether a key is genuine.
type Verifier struct {
	// An OpenAI-compatible base URL whose GET /models requires a key, e.g.
	// LM Studio's. Must be loopback or otherwise trusted: whatever answers
	// here decides who may use this router.
	BaseURL string `json:"base_url"`
	// How long an accepted key is trusted without re-asking. A key revoked
	// in the GUI keeps working until its entry expires, so keep this short
	// enough that revocation is not a surprise.
	TTLSeconds int `json:"ttl_seconds,omitempty"`
	// How long a rejection is remembered. This is what stops a brute force
	// from the internet turning into one upstream request per guess.
	NegativeTTLSeconds int `json:"negative_ttl_seconds,omitempty"`
}

func (v *Verifier) ttl() time.Duration {
	if v.TTLSeconds > 0 {
		return time.Duration(v.TTLSeconds) * time.Second
	}
	return 5 * time.Minute
}

func (v *Verifier) negativeTTL() time.Duration {
	if v.NegativeTTLSeconds > 0 {
		return time.Duration(v.NegativeTTLSeconds) * time.Second
	}
	return 30 * time.Second
}

type verdict struct {
	ok      bool
	expires time.Time
}

// verified caches what the verifier said, keyed by a hash of the presented
// key so no plaintext credential is held in the map. Bounded, and cleared
// wholesale when it grows: an unauthenticated endpoint on the internet must
// not be able to grow the router's memory one guess at a time.
var (
	verified   = map[string]verdict{}
	verifiedMu sync.Mutex
)

const verifiedCap = 1024

func askVerifier(v *Verifier, key string) bool {
	sum := sha256.Sum256([]byte(key))
	id := string(sum[:])

	verifiedMu.Lock()
	if e, ok := verified[id]; ok && time.Now().Before(e.expires) {
		verifiedMu.Unlock()
		return e.ok
	}
	verifiedMu.Unlock()

	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(v.BaseURL, "/")+"/models", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := httpClient.Do(req)
	if err != nil {
		// The verifier being unreachable must not lock the fleet out: fall
		// back to the static key list by answering "not verified" rather
		// than caching a rejection we cannot stand behind.
		log.Printf("key verifier %s unreachable: %v", v.BaseURL, err)
		return false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	ok := resp.StatusCode == http.StatusOK

	ttl := v.ttl()
	if !ok {
		ttl = v.negativeTTL()
	}
	verifiedMu.Lock()
	if len(verified) >= verifiedCap {
		verified = map[string]verdict{}
	}
	verified[id] = verdict{ok: ok, expires: time.Now().Add(ttl)}
	verifiedMu.Unlock()
	return ok
}

// clientKeys is every credential the front door accepts. Empty means the
// router is unauthenticated, which is only ever safe when it is bound to
// loopback.
func (c *Config) clientKeys() []string {
	keys := make([]string, 0, len(c.ClientAPIKeys)+2)
	if c.ClientAPIKey != "" {
		keys = append(keys, c.ClientAPIKey)
	}
	if c.ClientAPIKeyEnv != "" {
		if v := os.Getenv(c.ClientAPIKeyEnv); v != "" {
			keys = append(keys, v)
		}
	}
	for _, k := range c.ClientAPIKeys {
		if k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

func (c *Config) verifyModel() bool {
	return c.VerifyResponseModel == nil || *c.VerifyResponseModel
}

func (c *Config) dialTimeout() time.Duration {
	if c.DialTimeoutMS > 0 {
		return time.Duration(c.DialTimeoutMS) * time.Millisecond
	}
	return 2 * time.Second
}

func (c *Config) maxUpload() int64 {
	if c.MaxUploadBytes > 0 {
		return int64(c.MaxUploadBytes)
	}
	return 32 << 20
}

func (c *Config) headerTimeout() time.Duration {
	if c.ResponseHeaderTimeoutMS > 0 {
		return time.Duration(c.ResponseHeaderTimeoutMS) * time.Millisecond
	}
	return 10 * time.Minute
}

// current holds the live config. Swapped wholesale on reload so a request
// either sees the old routing table or the new one, never a half-applied mix.
var current atomic.Pointer[Config]

// httpClient serves both the proxy path and the key verifier.
var httpClient *http.Client

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	// Reject unknown fields: a typo in a key would otherwise be silently
	// ignored and leave the router doing something other than what the file
	// appears to say.
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if c.Listen == "" {
		return nil, fmt.Errorf("listen is required")
	}
	if len(c.Tiers) == 0 {
		return nil, fmt.Errorf("at least one tier is required")
	}
	for name, t := range c.Tiers {
		if t.BaseURL == "" || t.Model == "" {
			return nil, fmt.Errorf("tier %q needs both base_url and model", name)
		}
		u, err := url.Parse(t.BaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("tier %q has an unusable base_url %q", name, t.BaseURL)
		}
		if t.Kind != "" && t.Kind != kindChat && t.Kind != kindTranscription {
			return nil, fmt.Errorf("tier %q has unknown kind %q (want %q or %q)",
				name, t.Kind, kindChat, kindTranscription)
		}
	}
	if c.MaxUploadBytes < 0 {
		return nil, fmt.Errorf("max_upload_bytes must not be negative")
	}
	return &c, nil
}

// watch reloads the config when the file changes. A bad edit is logged and
// ignored: a typo must not take down working routing.
func watch(path string, every time.Duration) {
	stamp := func() string {
		fi, err := os.Stat(path)
		if err != nil {
			return "missing"
		}
		return fmt.Sprintf("%d-%d", fi.ModTime().UnixNano(), fi.Size())
	}
	last := stamp()
	for {
		time.Sleep(every)
		now := stamp()
		if now == last {
			continue
		}
		last = now
		c, err := loadConfig(path)
		if err != nil {
			log.Printf("reload REFUSED, keeping previous routing: %v", err)
			continue
		}
		current.Store(c)
		log.Printf("reloaded: %s", describe(c))
	}
}

func describe(c *Config) string {
	names := make([]string, 0, len(c.Tiers))
	for n := range c.Tiers {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		t := c.Tiers[n]
		line := fmt.Sprintf("%s -> %s [%s]", n, t.BaseURL, t.Model)
		if t.transcription() {
			line += " (" + kindTranscription + ")"
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, ", ")
}

func writeErr(w http.ResponseWriter, status int, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// OpenAI's error envelope, so existing clients surface it as an error
	// instead of a parse failure.
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "router_error"},
	})
}

// unleak replaces the backend's model name with the tier the client asked
// for, anywhere it appears as a "model" or "system_fingerprint" value.
//
// Without this the tier abstraction is only as good as a coincidence: LM
// Studio's identifiers happen to BE "amazing" and "great", so its echo looked
// clean, while Splash correctly echoed "incoai/Qwen3.8-27B-Splash" and told
// every client what was really behind the tier.
//
// Only an exact match on those keys is replaced, so generated text that
// happens to mention a model name is never altered — the router rewrites
// metadata, never content.
func unleak(v any, from, to string) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			// Synthetic names are rewritten too: oMLX opens a stream with a
			// keepalive frame whose model is literally "keepalive", which
			// otherwise reaches the client as a model name it never asked for.
			if s, ok := val.(string); ok && (s == from || syntheticStreamModel(s)) &&
				(k == "model" || k == "system_fingerprint") {
				t[k] = to
				continue
			}
			t[k] = unleak(val, from, to)
		}
		return t
	case []any:
		for i := range t {
			t[i] = unleak(t[i], from, to)
		}
		return t
	}
	return v
}

// streamBody replays any bytes a peek consumed ahead of the remaining body.
func streamBody(head []byte, rest io.Reader) io.Reader {
	if len(head) == 0 {
		return rest
	}
	return io.MultiReader(bytes.NewReader(head), rest)
}

// documentModel reads the top-level "model" of a JSON reply, "" if absent or
// unparseable. Top level only: nested copies echo the same value, and a
// "model" buried in generated content must not be mistaken for the answer.
func documentModel(raw []byte) string {
	var doc struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	return doc.Model
}

// syntheticStreamModel reports whether a streamed event names a placeholder
// rather than the model that will actually answer. oMLX opens a stream with a
// keepalive frame carrying `"model":"keepalive","created":0`, and taking that
// at face value made the substitution guard refuse every streaming request to
// the `instant` tier. Treat such a frame as "keep looking", not as an answer.
func syntheticStreamModel(m string) bool {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "keepalive", "ping", "heartbeat":
		return true
	}
	return false
}

// peekStreamModel reads an event stream only as far as the first data: event
// that names a model, and returns the bytes it consumed so they can be
// replayed. This has to happen before the status line is written, because
// after a 200 there is no way to turn the reply into an error.
func peekStreamModel(src io.Reader) ([]byte, string) {
	br := bufio.NewReaderSize(src, 64*1024)
	var head []byte
	// Scan a few events: the real model name can sit behind a synthetic
	// keepalive pre-roll. If none of them names a real model we return "",
	// and the caller skips the check rather than refusing valid traffic --
	// a genuine substitution still shows up on the frames that follow.
	for i := 0; i < 16; i++ {
		line, err := br.ReadBytes('\n')
		head = append(head, line...)
		if body, found := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte("data:")); found {
			payload := bytes.TrimLeft(body, " ")
			if m := documentModel(payload); m != "" && !syntheticStreamModel(m) {
				return append(head, drain(br)...), m
			}
		}
		if err != nil {
			break
		}
	}
	return append(head, drain(br)...), ""
}

// drain returns whatever the reader already buffered, so a peek never strands
// bytes that the caller still has to relay.
func drain(br *bufio.Reader) []byte {
	if n := br.Buffered(); n > 0 {
		buf := make([]byte, n)
		br.Read(buf)
		return buf
	}
	return nil
}

// rewriteJSON applies unleak to a JSON document, reporting whether it is still
// usable. Anything that does not parse is passed through untouched: a relay
// must never corrupt a body it does not understand.
func rewriteJSON(raw []byte, from, to string) ([]byte, bool) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return raw, false
	}
	out, err := json.Marshal(unleak(doc, from, to))
	if err != nil {
		return raw, false
	}
	return out, true
}

// relaySSE copies an event stream, rewriting each data: payload as it goes.
// Line at a time and flushed per line, so streaming stays streaming; a
// bufio.Reader rather than a Scanner because one chunk can exceed any token
// limit a Scanner would impose.
func relaySSE(dst io.Writer, src io.Reader, from, to string) (int64, error) {
	br := bufio.NewReaderSize(src, 64*1024)
	var written int64
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			out := line
			// Keep the exact terminator so the framing is byte-identical.
			body := line
			term := []byte{}
			for len(body) > 0 && (body[len(body)-1] == '\n' || body[len(body)-1] == '\r') {
				term = append([]byte{body[len(body)-1]}, term...)
				body = body[:len(body)-1]
			}
			if after, found := bytes.CutPrefix(body, []byte("data:")); found {
				payload := bytes.TrimLeft(after, " ")
				if rewritten, ok := rewriteJSON(payload, from, to); ok {
					out = append(append([]byte("data: "), rewritten...), term...)
				}
			}
			n, werr := dst.Write(out)
			written += int64(n)
			if werr != nil {
				return written, werr
			}
		}
		if err != nil {
			if err == io.EOF {
				return written, nil
			}
			return written, err
		}
	}
}

// cappedBuffer keeps at most limit bytes and remembers that it dropped the
// rest, so a captured response is never silently partial.
type cappedBuffer struct {
	limit   int
	b       []byte
	dropped bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.limit - len(c.b)
	switch {
	case room <= 0:
		c.dropped = len(p) > 0 || c.dropped
	case len(p) <= room:
		c.b = append(c.b, p...)
	default:
		c.b = append(c.b, p[:room]...)
		c.dropped = true
	}
	return len(p), nil
}

// flushWriter pushes every chunk straight to the client. Without this, SSE
// tokens would sit in a buffer and streaming would arrive in lumps.
type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

// hop-by-hop headers must not be forwarded.
var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true,
}

func authorized(c *Config, r *http.Request) bool {
	allowed := c.clientKeys()
	if len(allowed) == 0 && c.Verifier == nil {
		return true
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	ok := false
	for _, want := range allowed {
		// Constant time, and every candidate is checked: this endpoint is
		// published to the internet, so the comparison should not leak the
		// key through timing or through which candidate matched.
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
			ok = true
		}
	}
	if ok {
		return true
	}
	// Not one we were told about. Ask the backend that actually owns the
	// keys, so a key added in LM Studio's GUI works without editing anything
	// here. An empty bearer never justifies a round trip.
	if c.Verifier != nil && got != "" {
		return askVerifier(c.Verifier, got)
	}
	return false
}

// target maps an incoming path onto the backend. base_url carries the version
// prefix, so /v1/chat/completions becomes <base_url>/chat/completions.
func target(base, path string) string {
	return strings.TrimRight(base, "/") + strings.TrimPrefix(path, "/v1")
}

// An uploadedFile is what the audit log keeps of a file part: who sent what,
// how big, and a hash to match it against the original. The bytes themselves
// are never recorded.
type uploadedFile struct {
	Field       string `json:"field"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
	Bytes       int64  `json:"bytes"`
	SHA256      string `json:"sha256"`
}

// A multipartForm is what routing needs to know about an upload: every
// non-file field, and a summary of each file.
type multipartForm struct {
	fields map[string][]string
	files  []uploadedFile
}

// multipartBoundary reports whether the request is a multipart/form-data
// upload, and its boundary. Detection is by Content-Type alone, before any
// body is read, so the upload cap can be applied to exactly these requests.
func multipartBoundary(r *http.Request) (string, bool) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return "", false
	}
	return params["boundary"], true
}

// parseMultipart reads the fields and file summaries out of an upload. It uses
// NextRawPart so quoted-printable parts are never decoded: nothing here may
// alter bytes that will be forwarded. On error it still returns what it parsed
// so far, which the audit log can use.
func parseMultipart(body []byte, boundary string) (*multipartForm, error) {
	form := &multipartForm{fields: map[string][]string{}, files: []uploadedFile{}}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := mr.NextRawPart()
		if err == io.EOF {
			return form, nil
		}
		if err != nil {
			return form, err
		}
		if part.FileName() == "" {
			val, err := io.ReadAll(part)
			if err != nil {
				return form, err
			}
			form.fields[part.FormName()] = append(form.fields[part.FormName()], string(val))
			continue
		}
		h := sha256.New()
		n, err := io.Copy(h, part)
		if err != nil {
			return form, err
		}
		form.files = append(form.files, uploadedFile{
			Field:       part.FormName(),
			Filename:    part.FileName(),
			ContentType: part.Header.Get("Content-Type"),
			Bytes:       n,
			SHA256:      hex.EncodeToString(h.Sum(nil)),
		})
	}
}

// rewriteMultipart re-encodes an upload for the backend: the "model" field
// becomes the backend's model name and, if format is set, "response_format"
// becomes that. Every other part is copied with its headers and content bytes
// untouched, and the client's boundary is kept so the body stays as close to
// what was sent as the format allows. Part headers are written in sorted
// order, which may differ from the original; the content never does.
func rewriteMultipart(body []byte, boundary, model, format string) ([]byte, string, error) {
	var out bytes.Buffer
	mw := multipart.NewWriter(&out)
	// A boundary the writer will not accept (it is stricter than the RFC about
	// length and characters) falls back to the writer's own random one. Still
	// correct, since the Content-Type below names whichever is in use.
	if err := mw.SetBoundary(boundary); err != nil {
		log.Printf("multipart: keeping own boundary, client's is not reusable: %v", err)
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := mr.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", err
		}
		var replace *string
		if part.FileName() == "" {
			switch part.FormName() {
			case "model":
				replace = &model
			case "response_format":
				if format != "" {
					replace = &format
				}
			}
		}
		hdr := part.Header
		if replace != nil {
			// The new value is written as-is, so a transfer encoding that
			// described the old one would now be a lie.
			hdr.Del("Content-Transfer-Encoding")
		}
		w, err := mw.CreatePart(hdr)
		if err != nil {
			return nil, "", err
		}
		if replace != nil {
			if _, err := io.Copy(io.Discard, part); err != nil {
				return nil, "", err
			}
			_, err = io.WriteString(w, *replace)
		} else {
			_, err = io.Copy(w, part)
		}
		if err != nil {
			return nil, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	ctype := mime.FormatMediaType("multipart/form-data", map[string]string{"boundary": mw.Boundary()})
	if ctype == "" {
		ctype = "multipart/form-data; boundary=" + mw.Boundary()
	}
	return out.Bytes(), ctype, nil
}

// truthy reads a form boolean the way the backends do, loosely.
func truthy(v string) bool {
	if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
		return b
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "on":
		return true
	}
	return false
}

// auditBody picks how a request body is stored. Raw JSON only when it really is
// JSON: json.Marshal fails on an invalid RawMessage, and a record that cannot
// be marshalled is silently dropped, which loses exactly the malformed requests
// worth seeing.
func auditBody(body []byte, max int) (v any, truncated bool) {
	if len(body) > max {
		return string(body[:max]), true
	}
	if json.Valid(body) {
		return json.RawMessage(body), false
	}
	return string(body), false
}

// newHTTPClient builds the client for the proxy path and the key verifier.
func newHTTPClient(c *Config) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: c.dialTimeout()}).DialContext,
			ResponseHeaderTimeout: c.headerTimeout(),
			// Asking for gzip would invite the backend to buffer a stream
			// before compressing it, which is exactly what SSE must not do.
			DisableCompression: true,
		},
		// No overall deadline: a long generation is not a stuck one.
		Timeout: 0,
	}
}

func main() {
	configPath := flag.String("config", "", "path to the routing config (required)")
	flag.Parse()
	if *configPath == "" {
		log.Fatal("--config is required")
	}
	c, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	current.Store(c)
	if c.Audit != nil && c.Audit.Dir != "" {
		audit, err = newAuditLog(c.Audit)
		if err != nil {
			log.Fatalf("audit: %v", err)
		}
	}
	go watch(*configPath, 2*time.Second)

	httpClient = newHTTPClient(c)

	srv := &http.Server{
		Addr:    c.Listen,
		Handler: newMux(),
		// Both zero on purpose: a request may stream for an hour, and a
		// write deadline would cut a long generation off mid-sentence.
		ReadTimeout:  0,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}
	log.Printf("llm-router on %s", c.Listen)
	log.Printf("routing: %s", describe(c))
	if audit != nil {
		log.Printf("audit: %s, retain %d days, bodies=%v, responses=%v",
			c.Audit.Dir, c.Audit.retain(), c.Audit.bodies(), c.Audit.LogResponses)
	} else {
		log.Printf("audit: DISABLED (no audit.dir configured)")
	}
	log.Fatal(srv.ListenAndServe())
}

// newMux registers every route. It reads the live config, audit log and client
// through the package globals on each request, so a reload takes effect
// without rebuilding the mux.
func newMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	// What the router believes right now. Credentials are never echoed, but
	// the reply names every backend's address and real model -- exactly what
	// the tier abstraction keeps from clients -- so it sits behind the same
	// front door as every routed request, and is audited like one.
	mux.HandleFunc("/_tiers", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		cfg := current.Load()
		rec := record(cfg, r)
		if !authorized(cfg, r) {
			writeErr(w, http.StatusUnauthorized, "invalid or missing API key")
			finish(rec, start, http.StatusUnauthorized, "denied")
			return
		}
		defer finish(rec, start, http.StatusOK, "ok")
		out := map[string]any{}
		for n, t := range cfg.Tiers {
			out[n] = map[string]string{"base_url": t.BaseURL, "model": t.Model, "kind": t.kind()}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"tiers": out})
	})

	// Clients discover models here — Open WebUI builds its model list from
	// it — so it must advertise the tiers, not whatever the backends call
	// themselves.
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		cfg := current.Load()
		rec := record(cfg, r)
		if !authorized(cfg, r) {
			writeErr(w, http.StatusUnauthorized, "invalid or missing API key")
			finish(rec, start, http.StatusUnauthorized, "denied")
			return
		}
		defer finish(rec, start, http.StatusOK, "ok")
		names := make([]string, 0, len(cfg.Tiers))
		for n := range cfg.Tiers {
			names = append(names, n)
		}
		sort.Strings(names)
		data := make([]map[string]any, 0, len(names))
		for _, n := range names {
			data = append(data, map[string]any{
				"id": n, "object": "model", "created": 0, "owned_by": "llm-router",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		cfg := current.Load()
		rec := record(cfg, r)
		if !authorized(cfg, r) {
			writeErr(w, http.StatusUnauthorized, "invalid or missing API key")
			// A rejected key is the most interesting line in the file: it
			// records who tried, from where, with which key ID.
			finish(rec, start, http.StatusUnauthorized, "denied")
			return
		}
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusNotFound,
				"%s %s is not routed; this router serves GET /v1/models and POST endpoints carrying a \"model\" field",
				r.Method, r.URL.Path)
			finish(rec, start, http.StatusNotFound, "not_routed")
			return
		}
		// An upload is recognised by its Content-Type, before the body is
		// read, so the cap below applies to exactly those requests.
		boundary, isMultipart := multipartBoundary(r)
		src := r.Body
		if isMultipart {
			src = http.MaxBytesReader(w, r.Body, cfg.maxUpload())
		}
		body, err := io.ReadAll(src)
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeErr(w, http.StatusRequestEntityTooLarge,
					"upload exceeds the %d byte limit", tooBig.Limit)
				finish(rec, start, http.StatusRequestEntityTooLarge, "too_large")
				return
			}
			writeErr(w, http.StatusBadRequest, "could not read request body: %v", err)
			finish(rec, start, http.StatusBadRequest, "unreadable_body")
			return
		}
		if rec != nil {
			rec["req_bytes"] = len(body)
		}

		var requested string
		var form *multipartForm
		var fields map[string]json.RawMessage
		if isMultipart {
			var perr error
			form, perr = parseMultipart(body, boundary)
			if rec != nil {
				// File metadata always, and never the audio itself. The text
				// fields (prompt, language, ...) are the part that shows what
				// was asked for, so they follow the same log_bodies switch as
				// a JSON prompt does.
				rec["files"] = form.files
				if cfg.Audit != nil && cfg.Audit.bodies() {
					max := cfg.Audit.maxBody()
					req := map[string]any{}
					for name, vals := range form.fields {
						cut := make([]string, len(vals))
						for i, v := range vals {
							if len(v) > max {
								v = v[:max]
							}
							cut[i] = v
						}
						if len(cut) == 1 {
							req[name] = cut[0]
						} else {
							req[name] = cut
						}
					}
					rec["request"] = req
				}
			}
			if perr != nil {
				writeErr(w, http.StatusBadRequest, "request body is not valid multipart/form-data: %v", perr)
				finish(rec, start, http.StatusBadRequest, "bad_multipart")
				return
			}
			models := form.fields["model"]
			if len(models) > 1 {
				writeErr(w, http.StatusBadRequest, "request has more than one \"model\" field")
				finish(rec, start, http.StatusBadRequest, "bad_multipart")
				return
			}
			if len(models) == 1 {
				requested = models[0]
			}
		} else {
			if rec != nil && cfg.Audit != nil && cfg.Audit.bodies() {
				// The prompt itself, as raw JSON so it stays structured in
				// the record rather than becoming an escaped blob. Truncated
				// with a marker so a record is never silently partial.
				v, truncated := auditBody(body, cfg.Audit.maxBody())
				rec["request"] = v
				if truncated {
					rec["request_truncated"] = true
				}
			}
			// Decode into raw values so every field but "model" is forwarded
			// byte-for-byte: no number reformatting, no dropped keys.
			if err := json.Unmarshal(body, &fields); err != nil {
				writeErr(w, http.StatusBadRequest, "request body is not a JSON object: %v", err)
				finish(rec, start, http.StatusBadRequest, "bad_json")
				return
			}
			if raw, ok := fields["model"]; ok {
				json.Unmarshal(raw, &requested)
			}
		}
		if requested == "" {
			writeErr(w, http.StatusBadRequest, "request has no \"model\" field to route on")
			finish(rec, start, http.StatusBadRequest, "no_model")
			return
		}
		tier, ok := cfg.Tiers[requested]
		if !ok {
			names := make([]string, 0, len(cfg.Tiers))
			for n := range cfg.Tiers {
				names = append(names, n)
			}
			sort.Strings(names)
			writeErr(w, http.StatusNotFound,
				"unknown model %q; this router serves: %s", requested, strings.Join(names, ", "))
			if rec != nil {
				rec["tier"] = requested
			}
			finish(rec, start, http.StatusNotFound, "unknown_tier")
			return
		}
		if rec != nil {
			rec["tier"] = requested
			rec["backend"] = tier.BaseURL
			rec["backend_model"] = tier.Model
		}
		// Which endpoint a tier may be used on is part of its definition. A chat
		// tier handed to the audio path used to reach the backend, which
		// answered 200 with an error document (or a 404/422 naming its own
		// internals); refusing here keeps backend text out of the reply and the
		// audit outcome honest.
		if tier.transcription() {
			switch {
			case r.URL.Path != transcriptionPath:
				writeErr(w, http.StatusBadRequest,
					"tier %q is a transcription tier: POST %s with a multipart/form-data body", requested, transcriptionPath)
				finish(rec, start, http.StatusBadRequest, "wrong_endpoint")
				return
			case !isMultipart:
				writeErr(w, http.StatusBadRequest,
					"tier %q: transcription requests must be multipart/form-data", requested)
				finish(rec, start, http.StatusBadRequest, "bad_multipart")
				return
			}
		} else if isMultipart || strings.HasPrefix(r.URL.Path, "/v1/audio/") {
			writeErr(w, http.StatusBadRequest,
				"tier %q is a chat tier: send JSON to its chat endpoints such as /v1/chat/completions; audio transcription is served by a transcription tier at %s",
				requested, transcriptionPath)
			finish(rec, start, http.StatusBadRequest, "wrong_endpoint")
			return
		}

		// response_format: the backend always answers JSON whatever is asked,
		// so the router supports what it can honour. "text" is plain text
		// derived from that JSON; everything else needs a backend that renders
		// it, and failing loudly beats returning JSON to a client expecting SRT.
		var wantText, streaming bool
		if tier.transcription() {
			formats := form.fields["response_format"]
			if len(formats) > 1 {
				writeErr(w, http.StatusBadRequest, "request has more than one \"response_format\" field")
				finish(rec, start, http.StatusBadRequest, "bad_multipart")
				return
			}
			if len(formats) == 1 {
				switch formats[0] {
				case "", "json":
				case "text":
					wantText = true
				default:
					writeErr(w, http.StatusBadRequest,
						"tier %q supports response_format \"json\" and \"text\", not %q", requested, formats[0])
					finish(rec, start, http.StatusBadRequest, "unsupported_format")
					return
				}
			}
			for _, v := range form.fields["stream"] {
				streaming = streaming || truthy(v)
			}
		}

		var outBody []byte
		var outType string
		if isMultipart {
			format := ""
			if wantText {
				// Ask the backend for JSON and derive the text from it.
				format = "json"
			}
			outBody, outType, err = rewriteMultipart(body, boundary, tier.Model, format)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "request body is not valid multipart/form-data: %v", err)
				finish(rec, start, http.StatusBadRequest, "bad_multipart")
				return
			}
		} else {
			// The one rewrite that makes tiers work.
			renamed, err := json.Marshal(tier.Model)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "could not encode model name: %v", err)
				finish(rec, start, http.StatusInternalServerError, "encode_failed")
				return
			}
			fields["model"] = renamed
			outBody, err = json.Marshal(fields)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "could not re-encode request: %v", err)
				finish(rec, start, http.StatusInternalServerError, "encode_failed")
				return
			}
			outType = "application/json"
		}

		dest := target(tier.BaseURL, r.URL.Path)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, dest, bytes.NewReader(outBody))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "could not build backend request: %v", err)
			finish(rec, start, http.StatusInternalServerError, "build_failed")
			return
		}
		for name, values := range r.Header {
			canonical := http.CanonicalHeaderKey(name)
			// Authorization is replaced with the backend's own credential;
			// Content-Length is recomputed after the model rewrite; and
			// Accept-Encoding is dropped so a backend cannot gzip — and
			// therefore buffer — a stream the client is waiting on token by
			// token.
			if hopByHop[canonical] || canonical == "Authorization" ||
				canonical == "Content-Length" || canonical == "Accept-Encoding" {
				continue
			}
			for _, v := range values {
				req.Header.Add(name, v)
			}
		}
		req.Header.Set("Content-Type", outType)
		req.ContentLength = int64(len(outBody))
		if k := tier.key(); k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			// Name the tier AND the backend: when this fires, the question is
			// always "which box is down", and a bare proxy error hides it.
			writeErr(w, http.StatusBadGateway,
				"tier %q backend %s is not answering: %v", requested, tier.BaseURL, err)
			if rec != nil {
				rec["error"] = err.Error()
			}
			finish(rec, start, http.StatusBadGateway, "backend_unreachable")
			return
		}
		defer resp.Body.Close()

		// Does the reply need the backend's name taken back out? Only when the
		// backend calls the model something else, and only for bodies we can
		// parse. Rewriting changes the length, so Content-Length cannot be
		// forwarded verbatim.
		ctype := resp.Header.Get("Content-Type")
		isSSE := strings.Contains(ctype, "text/event-stream")
		isJSON := strings.Contains(ctype, "application/json")
		//
		// Not for a transcription tier: its replies name no model, so there is
		// nothing to rewrite, and the substitution peek below would hold back up
		// to 16 lines of an event stream looking for one.
		unleaking := !tier.transcription() && tier.Model != requested && (isSSE || isJSON)

		// Whether to assert the backend answered as the model we asked for.
		// Only for successful, parseable replies: an error body has no model
		// to check.
		checking := !tier.transcription() && cfg.verifyModel() && resp.StatusCode < 400 && (isSSE || isJSON)

		// response_format=text: only a successful, non-streamed JSON reply is
		// converted. An error keeps its envelope and status so the client sees
		// what went wrong, and a stream has no single text to return.
		convertText := wantText && !streaming && resp.StatusCode >= 200 && resp.StatusCode < 300 && isJSON

		var buffered []byte
		var converted bool
		var sseHead []byte
		var served string

		switch {
		case convertText:
			raw, readErr := io.ReadAll(resp.Body)
			buffered = raw
			if readErr == nil {
				var doc struct {
					Text *string `json:"text"`
				}
				// No string "text" means this is not the reply we know how to
				// convert, so it goes through untouched.
				if json.Unmarshal(raw, &doc) == nil && doc.Text != nil {
					buffered = append([]byte{}, *doc.Text...)
					converted = true
				}
			}
		case (unleaking || checking) && !isSSE:
			// A non-streamed reply is one bounded JSON document, so buffer it,
			// inspect, rewrite, and send a correct Content-Length. Nothing is
			// being streamed here, so nothing is delayed by waiting for it.
			raw, readErr := io.ReadAll(resp.Body)
			if readErr == nil {
				served = documentModel(raw)
				if unleaking {
					buffered, _ = rewriteJSON(raw, tier.Model, requested)
				} else {
					buffered = raw
				}
			} else {
				buffered = raw
			}
		case checking && isSSE:
			// Peek just far enough to read the model out of the first event,
			// BEFORE the status line goes out — once a 200 is written we can
			// no longer turn the reply into an error. The peeked bytes are
			// replayed, so nothing is lost and nothing is buffered beyond the
			// first event.
			sseHead, served = peekStreamModel(resp.Body)
		}

		// A backend that answers as a different model is serving something
		// other than the tier that was asked for. LM Studio does exactly this
		// when the requested identifier is not loaded: on 2026-10-18 `amazing`
		// crashed and every request for it was quietly answered by `great`,
		// with nothing downstream any the wiser. Fail loudly instead.
		if checking && served != "" && served != requested && served != tier.Model {
			writeErr(w, http.StatusBadGateway,
				"tier %q: backend %s answered as model %q, expected %q — the tier's model is probably not loaded",
				requested, tier.BaseURL, served, tier.Model)
			if rec != nil {
				rec["served_model"] = served
			}
			finish(rec, start, http.StatusBadGateway, "model_substituted")
			log.Printf("REFUSED substitution: tier %q expected %q, backend %s answered %q",
				requested, tier.Model, tier.BaseURL, served)
			return
		}

		for name, values := range resp.Header {
			canonical := http.CanonicalHeaderKey(name)
			if hopByHop[canonical] {
				continue
			}
			if (unleaking || converted) && canonical == "Content-Length" {
				continue
			}
			if converted && canonical == "Content-Type" {
				continue
			}
			for _, v := range values {
				w.Header().Add(name, v)
			}
		}
		// Tell anything in front of us not to buffer the stream either.
		w.Header().Set("X-Accel-Buffering", "no")
		if buffered != nil {
			w.Header().Set("Content-Length", fmt.Sprint(len(buffered)))
		}
		status := resp.StatusCode
		if converted {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			status = http.StatusOK
		}
		w.WriteHeader(status)
		f, _ := w.(http.Flusher)
		var dst io.Writer = flushWriter{w: w, f: f}
		var capture *cappedBuffer
		if rec != nil && cfg.Audit != nil && cfg.Audit.LogResponses {
			capture = &cappedBuffer{limit: cfg.Audit.maxBody()}
			dst = io.MultiWriter(dst, capture)
		}
		var n int64
		switch {
		case buffered != nil:
			m, _ := dst.Write(buffered)
			n = int64(m)
		case unleaking && isSSE:
			n, _ = relaySSE(dst, streamBody(sseHead, resp.Body), tier.Model, requested)
		default:
			n, _ = io.Copy(dst, streamBody(sseHead, resp.Body))
		}
		if rec != nil {
			rec["unleaked"] = unleaking
		}
		if rec != nil {
			rec["resp_bytes"] = n
			if capture != nil {
				rec["response"] = string(capture.b)
				if capture.dropped {
					rec["response_truncated"] = true
				}
			}
		}
		outcome := "ok"
		if resp.StatusCode >= 400 {
			outcome = "backend_error"
		}
		finish(rec, start, resp.StatusCode, outcome)
	})

	return mux
}
