package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/rand"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- harness ---------------------------------------------------------------

const testKey = "test-client-key"

// seen is what a stub backend received.
type seen struct {
	Path        string
	ContentType string
	Auth        string
	Length      int64
	Body        []byte
}

// stub is a backend that records its last request and answers with handler.
type stub struct {
	*httptest.Server
	mu   sync.Mutex
	last seen
	hits int
}

func newStub(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body []byte)) *stub {
	t.Helper()
	s := &stub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.last = seen{
			Path: r.URL.Path, ContentType: r.Header.Get("Content-Type"),
			Auth: r.Header.Get("Authorization"), Length: r.ContentLength, Body: body,
		}
		s.hits++
		s.mu.Unlock()
		handler(w, r, body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stub) request() seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *stub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

// router starts the real mux against cfg, with the globals the handlers read
// pointed at it for the length of the test. Tests here are sequential, as the
// globals require.
func router(t *testing.T, cfg *Config) *httptest.Server {
	t.Helper()
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:0"
	}
	if len(cfg.ClientAPIKeys) == 0 {
		cfg.ClientAPIKeys = []string{testKey}
	}
	oldCur, oldClient, oldAudit := current.Load(), httpClient, audit
	current.Store(cfg)
	httpClient = newHTTPClient(cfg)
	audit = nil
	if cfg.Audit != nil {
		a, err := newAuditLog(cfg.Audit)
		if err != nil {
			t.Fatal(err)
		}
		audit = a
	}
	srv := httptest.NewServer(newMux())
	t.Cleanup(func() {
		srv.Close()
		if audit != nil {
			audit.mu.Lock()
			if audit.file != nil {
				audit.file.Close()
			}
			audit.mu.Unlock()
		}
		current.Store(oldCur)
		httpClient, audit = oldClient, oldAudit
	})
	return srv
}

// auditRecords waits for n records (a record is written just after the reply
// is, so the client can be ahead of it) and returns them all.
func auditRecords(t *testing.T, dir string, n int) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for deadline := time.Now().Add(3 * time.Second); ; {
		recs = nil
		files, _ := filepath.Glob(filepath.Join(dir, "audit-*.jsonl"))
		for _, f := range files {
			raw, _ := os.ReadFile(f)
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				if line == "" {
					continue
				}
				var rec map[string]any
				if err := json.Unmarshal([]byte(line), &rec); err != nil {
					t.Fatalf("audit line is not JSON: %v\n%s", err, line)
				}
				recs = append(recs, rec)
			}
		}
		if len(recs) >= n || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(recs) < n {
		t.Fatalf("want %d audit records, have %d: %v", n, len(recs), recs)
	}
	return recs
}

func lastOutcome(t *testing.T, dir string, n int) string {
	t.Helper()
	recs := auditRecords(t, dir, n)
	return recs[len(recs)-1]["outcome"].(string)
}

type formPart struct {
	name, filename, ctype string
	data                  []byte
}

const testBoundary = "testboundary-8f3a91c2"

func buildMultipart(t *testing.T, boundary string, parts []formPart) (body []byte, ctype string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.SetBoundary(boundary); err != nil {
		t.Fatal(err)
	}
	for _, p := range parts {
		var w io.Writer
		var err error
		if p.filename == "" {
			w, err = mw.CreateFormField(p.name)
		} else {
			h := map[string][]string{
				"Content-Disposition": {`form-data; name="` + p.name + `"; filename="` + p.filename + `"`},
				"Content-Type":        {p.ctype},
			}
			w, err = mw.CreatePart(h)
		}
		if err != nil {
			t.Fatal(err)
		}
		w.Write(p.data)
	}
	mw.Close()
	return buf.Bytes(), "multipart/form-data; boundary=" + boundary
}

// fakeAudio is binary content that is awkward on purpose: CRLFs, dashes, NULs.
func fakeAudio(n int) []byte {
	rnd := rand.New(rand.NewSource(7))
	b := make([]byte, n)
	rnd.Read(b)
	copy(b, "AUDIOMARKER\r\n--not-a-boundary\r\n\x00\x01")
	return b
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// echoBackend answers a transcription upload with what it received.
func echoBackend(t *testing.T) *stub {
	return newStub(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		out := map[string]any{"text": "hello world", "language": "en"}
		_, params, perr := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		var err error = perr
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(422)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": err.Error()}})
			return
		}
		fields := map[string]string{}
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			data, _ := io.ReadAll(p)
			if p.FileName() != "" {
				out["file_sha256"] = sum(data)
				out["file_name"] = p.FileName()
				out["file_type"] = p.Header.Get("Content-Type")
			} else {
				fields[p.FormName()] = string(data)
			}
		}
		out["fields"] = fields
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
}

func transcribeCfg(back *stub) *Config {
	return &Config{Tiers: map[string]Tier{
		"transcribe-great": {BaseURL: back.URL + "/v1", Model: "whisper-backend", Kind: "transcription", APIKey: "backend-secret"},
	}}
}

func post(t *testing.T, url, ctype string, body []byte, auth bool) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+testKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func errMessage(raw []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(raw, &env)
	return env.Error.Message
}

// --- multipart forwarding --------------------------------------------------

func TestMultipartForwarding(t *testing.T) {
	audio := fakeAudio(300 * 1024)
	for _, tc := range []struct {
		name  string
		parts []formPart
	}{
		{"model before file", []formPart{
			{name: "model", data: []byte("transcribe-great")},
			{name: "language", data: []byte("fr")},
			{name: "file", filename: "clip.wav", ctype: "audio/wav", data: audio},
			{name: "prompt", data: []byte("Accents: café")},
		}},
		{"model after file", []formPart{
			{name: "file", filename: "clip.wav", ctype: "audio/wav", data: audio},
			{name: "language", data: []byte("fr")},
			{name: "prompt", data: []byte("Accents: café")},
			{name: "model", data: []byte("transcribe-great")},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			back := echoBackend(t)
			srv := router(t, transcribeCfg(back))
			body, ctype := buildMultipart(t, testBoundary, tc.parts)

			resp, raw := post(t, srv.URL+"/v1/audio/transcriptions", ctype, body, true)
			if resp.StatusCode != 200 {
				t.Fatalf("status %d: %s", resp.StatusCode, raw)
			}
			var got struct {
				Text       string            `json:"text"`
				FileSHA256 string            `json:"file_sha256"`
				FileName   string            `json:"file_name"`
				FileType   string            `json:"file_type"`
				Fields     map[string]string `json:"fields"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got.FileSHA256 != sum(audio) {
				t.Errorf("file bytes changed: %s != %s", got.FileSHA256, sum(audio))
			}
			if got.FileName != "clip.wav" || got.FileType != "audio/wav" {
				t.Errorf("file metadata changed: %q %q", got.FileName, got.FileType)
			}
			if got.Fields["model"] != "whisper-backend" {
				t.Errorf("model not rewritten: %q", got.Fields["model"])
			}
			if got.Fields["language"] != "fr" || got.Fields["prompt"] != "Accents: café" {
				t.Errorf("other fields changed: %v", got.Fields)
			}
			if len(got.Fields) != 3 {
				t.Errorf("unexpected fields: %v", got.Fields)
			}
			// The reply is passed through, not unleaked or reinterpreted.
			if got.Text != "hello world" {
				t.Errorf("reply altered: %s", raw)
			}

			rq := back.request()
			if rq.Path != "/v1/audio/transcriptions" {
				t.Errorf("backend path %q", rq.Path)
			}
			mt, params, err := mime.ParseMediaType(rq.ContentType)
			if err != nil || mt != "multipart/form-data" || params["boundary"] != testBoundary {
				t.Errorf("boundary not preserved: %q", rq.ContentType)
			}
			if rq.Length != int64(len(rq.Body)) {
				t.Errorf("Content-Length %d, body %d", rq.Length, len(rq.Body))
			}
			if rq.Auth != "Bearer backend-secret" {
				t.Errorf("backend credential not applied: %q", rq.Auth)
			}
		})
	}
}

func TestMultipartUnusableBoundaryFallsBack(t *testing.T) {
	// 71 characters is over the writer's limit, so it cannot be reused.
	boundary := strings.Repeat("b", 71)
	var raw bytes.Buffer
	raw.WriteString("--" + boundary + "\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ntranscribe-great\r\n")
	raw.WriteString("--" + boundary + "\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.wav\"\r\n\r\nxyz\r\n")
	raw.WriteString("--" + boundary + "--\r\n")

	back := echoBackend(t)
	srv := router(t, transcribeCfg(back))
	resp, out := post(t, srv.URL+"/v1/audio/transcriptions", "multipart/form-data; boundary="+boundary, raw.Bytes(), true)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}
	if strings.Contains(back.request().ContentType, boundary) {
		t.Errorf("expected a fresh boundary, got %q", back.request().ContentType)
	}
	if !strings.Contains(string(out), sum([]byte("xyz"))) {
		t.Errorf("file did not arrive intact: %s", out)
	}
}

// --- multipart errors ------------------------------------------------------

func TestMultipartErrors(t *testing.T) {
	audio := fakeAudio(4096)
	file := formPart{name: "file", filename: "a.wav", ctype: "audio/wav", data: audio}

	cases := []struct {
		name    string
		parts   []formPart
		status  int
		outcome string
	}{
		{"missing model", []formPart{file, {name: "language", data: []byte("en")}}, 400, "no_model"},
		{"empty model", []formPart{file, {name: "model", data: nil}}, 400, "no_model"},
		{"duplicate model", []formPart{
			{name: "model", data: []byte("transcribe-great")}, file,
			{name: "model", data: []byte("transcribe-great")}}, 400, "bad_multipart"},
		{"unknown tier", []formPart{file, {name: "model", data: []byte("nope")}}, 404, "unknown_tier"},
		{"model sent as a file does not count", []formPart{
			{name: "model", filename: "m.txt", ctype: "text/plain", data: []byte("transcribe-great")}}, 400, "no_model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			back := echoBackend(t)
			cfg := transcribeCfg(back)
			cfg.Audit = &Audit{Dir: t.TempDir()}
			srv := router(t, cfg)
			body, ctype := buildMultipart(t, testBoundary, tc.parts)
			resp, raw := post(t, srv.URL+"/v1/audio/transcriptions", ctype, body, true)
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d, want %d: %s", resp.StatusCode, tc.status, raw)
			}
			if errMessage(raw) == "" {
				t.Errorf("no OpenAI error envelope: %s", raw)
			}
			if got := lastOutcome(t, cfg.Audit.Dir, 1); got != tc.outcome {
				t.Errorf("outcome %q, want %q", got, tc.outcome)
			}
			if back.count() != 0 {
				t.Errorf("a refused request reached the backend")
			}
		})
	}

	t.Run("unparseable body", func(t *testing.T) {
		back := echoBackend(t)
		cfg := transcribeCfg(back)
		cfg.Audit = &Audit{Dir: t.TempDir()}
		srv := router(t, cfg)
		resp, raw := post(t, srv.URL+"/v1/audio/transcriptions",
			"multipart/form-data; boundary=zzz", []byte("this is not multipart"), true)
		if resp.StatusCode != 400 {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		// Not "no_model": a body that cannot be read is not a missing field.
		if got := lastOutcome(t, cfg.Audit.Dir, 1); got != "bad_multipart" {
			t.Errorf("outcome %q", got)
		}
	})

	t.Run("truncated body", func(t *testing.T) {
		back := echoBackend(t)
		cfg := transcribeCfg(back)
		cfg.Audit = &Audit{Dir: t.TempDir()}
		srv := router(t, cfg)
		body, ctype := buildMultipart(t, testBoundary, []formPart{
			{name: "model", data: []byte("transcribe-great")}, file})
		// Cut the closing boundary and part of the file off.
		resp, raw := post(t, srv.URL+"/v1/audio/transcriptions", ctype, body[:len(body)-2000], true)
		if resp.StatusCode != 400 {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		if got := lastOutcome(t, cfg.Audit.Dir, 1); got != "bad_multipart" {
			t.Errorf("outcome %q", got)
		}
	})

	t.Run("over the cap", func(t *testing.T) {
		back := echoBackend(t)
		cfg := transcribeCfg(back)
		cfg.MaxUploadBytes = 2048
		cfg.Audit = &Audit{Dir: t.TempDir()}
		srv := router(t, cfg)
		body, ctype := buildMultipart(t, testBoundary, []formPart{
			{name: "model", data: []byte("transcribe-great")}, file})
		resp, raw := post(t, srv.URL+"/v1/audio/transcriptions", ctype, body, true)
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		if got := lastOutcome(t, cfg.Audit.Dir, 1); got != "too_large" {
			t.Errorf("outcome %q", got)
		}
		if back.count() != 0 {
			t.Errorf("an oversized upload reached the backend")
		}
	})

	t.Run("the cap is multipart only", func(t *testing.T) {
		back := newStub(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"model":"chat-backend","ok":true}`)
		})
		cfg := &Config{MaxUploadBytes: 1024, Tiers: map[string]Tier{
			"great": {BaseURL: back.URL + "/v1", Model: "chat-backend"}}}
		srv := router(t, cfg)
		big := `{"model":"great","messages":[{"role":"user","content":"` + strings.Repeat("x", 8192) + `"}]}`
		resp, raw := post(t, srv.URL+"/v1/chat/completions", "application/json", []byte(big), true)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
	})

	t.Run("json body to a transcription tier", func(t *testing.T) {
		back := echoBackend(t)
		cfg := transcribeCfg(back)
		cfg.Audit = &Audit{Dir: t.TempDir()}
		srv := router(t, cfg)
		resp, raw := post(t, srv.URL+"/v1/audio/transcriptions", "application/json",
			[]byte(`{"model":"transcribe-great"}`), true)
		if resp.StatusCode != 400 {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		if !strings.Contains(errMessage(raw), "multipart/form-data") {
			t.Errorf("message does not say what is needed: %q", errMessage(raw))
		}
		if got := lastOutcome(t, cfg.Audit.Dir, 1); got != "bad_multipart" {
			t.Errorf("outcome %q", got)
		}
		if back.count() != 0 {
			t.Errorf("request reached the backend")
		}
	})
}

// --- kinds -----------------------------------------------------------------

func TestKindMismatches(t *testing.T) {
	back := echoBackend(t)
	chat := newStub(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"chat-backend"}`)
	})
	cfg := &Config{Audit: &Audit{Dir: t.TempDir()}, Tiers: map[string]Tier{
		"transcribe-great": {BaseURL: back.URL + "/v1", Model: "whisper-backend", Kind: "transcription"},
		"great":            {BaseURL: chat.URL + "/v1", Model: "chat-backend"},
		"explicit":         {BaseURL: chat.URL + "/v1", Model: "chat-backend", Kind: "chat"},
	}}
	srv := router(t, cfg)

	mp, mpType := buildMultipart(t, testBoundary, []formPart{
		{name: "model", data: []byte("transcribe-great")},
		{name: "file", filename: "a.wav", ctype: "audio/wav", data: fakeAudio(100)}})
	mpChat, mpChatType := buildMultipart(t, testBoundary, []formPart{
		{name: "model", data: []byte("great")},
		{name: "file", filename: "a.wav", ctype: "audio/wav", data: fakeAudio(100)}})

	cases := []struct {
		name, path, ctype string
		body              []byte
		wantInMsg         string
	}{
		{"transcription tier at chat completions", "/v1/chat/completions", "application/json",
			[]byte(`{"model":"transcribe-great","messages":[]}`), "/v1/audio/transcriptions"},
		{"transcription tier at chat completions, multipart", "/v1/chat/completions", mpType, mp,
			"/v1/audio/transcriptions"},
		{"transcription tier at another audio path", "/v1/audio/translations", mpType, mp,
			"/v1/audio/transcriptions"},
		{"chat tier at the audio path, json", "/v1/audio/transcriptions", "application/json",
			[]byte(`{"model":"great"}`), `"great"`},
		{"chat tier at the audio path, multipart", "/v1/audio/transcriptions", mpChatType, mpChat, `"great"`},
		{"chat tier at another audio path", "/v1/audio/speech", "application/json",
			[]byte(`{"model":"great"}`), `"great"`},
		{"multipart body to a chat tier", "/v1/chat/completions", mpChatType, mpChat, `"great"`},
		{"explicit chat kind behaves like the default", "/v1/audio/transcriptions", "application/json",
			[]byte(`{"model":"explicit"}`), `"explicit"`},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := post(t, srv.URL+tc.path, tc.ctype, tc.body, true)
			if resp.StatusCode != 400 {
				t.Fatalf("status %d: %s", resp.StatusCode, raw)
			}
			if !strings.Contains(errMessage(raw), tc.wantInMsg) {
				t.Errorf("message %q does not mention %q", errMessage(raw), tc.wantInMsg)
			}
			if got := lastOutcome(t, cfg.Audit.Dir, i+1); got != "wrong_endpoint" {
				t.Errorf("outcome %q", got)
			}
		})
	}
	if back.count() != 0 || chat.count() != 0 {
		t.Errorf("a refused request reached a backend (%d, %d)", back.count(), chat.count())
	}
}

func TestInvalidKindFailsToLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "tiers.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := loadConfig(write(`{"listen":"127.0.0.1:1","tiers":{"x":{"base_url":"http://127.0.0.1:2/v1","model":"m","kind":"speech"}}}`)); err == nil {
		t.Error("kind \"speech\" should be rejected")
	}
	for _, kind := range []string{"", `,"kind":"chat"`, `,"kind":"transcription"`} {
		c, err := loadConfig(write(`{"listen":"127.0.0.1:1","tiers":{"x":{"base_url":"http://127.0.0.1:2/v1","model":"m"` + kind + `}}}`))
		if err != nil {
			t.Errorf("kind %q should load: %v", kind, err)
			continue
		}
		if want := strings.Contains(kind, "transcription"); c.Tiers["x"].transcription() != want {
			t.Errorf("kind %q: transcription()=%v", kind, !want)
		}
	}
	if _, err := loadConfig(write(`{"listen":"127.0.0.1:1","max_upload_bytes":-1,"tiers":{"x":{"base_url":"http://127.0.0.1:2/v1","model":"m"}}}`)); err == nil {
		t.Error("negative max_upload_bytes should be rejected")
	}
	c, err := loadConfig(write(`{"listen":"127.0.0.1:1","tiers":{"x":{"base_url":"http://127.0.0.1:2/v1","model":"m"}}}`))
	if err != nil || c.maxUpload() != 32<<20 {
		t.Errorf("default upload cap: %v %v", c, err)
	}
}

func TestTiersAndModelsListing(t *testing.T) {
	cfg := &Config{Tiers: map[string]Tier{
		"transcribe-great": {BaseURL: "http://127.0.0.1:1/v1", Model: "w", Kind: "transcription"},
		"great":            {BaseURL: "http://127.0.0.1:1/v1", Model: "g"},
	}}
	srv := router(t, cfg)

	get := func(path string) []byte {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return raw
	}
	var tiers struct {
		Tiers map[string]map[string]string `json:"tiers"`
	}
	json.Unmarshal(get("/_tiers"), &tiers)
	if tiers.Tiers["transcribe-great"]["kind"] != "transcription" || tiers.Tiers["great"]["kind"] != "chat" {
		t.Errorf("/_tiers kinds: %v", tiers.Tiers)
	}
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.Unmarshal(get("/v1/models"), &models)
	if len(models.Data) != 2 {
		t.Errorf("/v1/models should list every tier: %v", models.Data)
	}
	if d := describe(cfg); !strings.Contains(d, "transcribe-great -> http://127.0.0.1:1/v1 [w] (transcription)") ||
		strings.Contains(d, "[g] (") {
		t.Errorf("describe: %s", d)
	}
}

// --- response_format -------------------------------------------------------

func uploadWith(t *testing.T, extra ...formPart) ([]byte, string) {
	parts := []formPart{
		{name: "model", data: []byte("transcribe-great")},
		{name: "file", filename: "a.wav", ctype: "audio/wav", data: fakeAudio(2048)},
	}
	return buildMultipart(t, testBoundary, append(parts, extra...))
}

func TestResponseFormat(t *testing.T) {
	jsonBackend := func(t *testing.T, status int, ctype, reply string) *stub {
		return newStub(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
			w.Header().Set("Content-Type", ctype)
			w.WriteHeader(status)
			io.WriteString(w, reply)
		})
	}
	const transcript = "Hello, café.\nSecond line."

	t.Run("text becomes text/plain", func(t *testing.T) {
		reply, _ := json.Marshal(map[string]any{"text": transcript, "language": "en"})
		back := jsonBackend(t, 200, "application/json", string(reply))
		srv := router(t, transcribeCfg(back))
		body, ctype := uploadWith(t, formPart{name: "response_format", data: []byte("text")})
		resp, raw := post(t, srv.URL+"/v1/audio/transcriptions", ctype, body, true)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		if got := resp.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
			t.Errorf("Content-Type %q", got)
		}
		if string(raw) != transcript {
			t.Errorf("body %q", raw)
		}
		if resp.ContentLength != int64(len(transcript)) {
			t.Errorf("Content-Length %d, want %d", resp.ContentLength, len(transcript))
		}
		// The backend was asked for JSON, which is all it can produce.
		mr := multipart.NewReader(bytes.NewReader(back.request().Body), testBoundary)
		for {
			p, err := mr.NextPart()
			if err != nil {
				t.Fatal("response_format part missing in forwarded body")
			}
			if p.FormName() == "response_format" {
				v, _ := io.ReadAll(p)
				if string(v) != "json" {
					t.Errorf("backend got response_format %q", v)
				}
				break
			}
		}
	})

	t.Run("json and absent pass through", func(t *testing.T) {
		const reply = `{"text":"hi","language":"en"}`
		back := jsonBackend(t, 200, "application/json", reply)
		srv := router(t, transcribeCfg(back))
		for _, extra := range [][]formPart{nil, {{name: "response_format", data: []byte("json")}}, {{name: "response_format", data: nil}}} {
			body, ctype := uploadWith(t, extra...)
			resp, raw := post(t, srv.URL+"/v1/audio/transcriptions", ctype, body, true)
			if resp.StatusCode != 200 || string(raw) != reply ||
				!strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
				t.Errorf("not passed through: %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), raw)
			}
		}
	})

	t.Run("unsupported formats are refused", func(t *testing.T) {
		back := jsonBackend(t, 200, "application/json", `{"text":"hi"}`)
		cfg := transcribeCfg(back)
		cfg.Audit = &Audit{Dir: t.TempDir()}
		srv := router(t, cfg)
		for i, f := range []string{"srt", "vtt", "verbose_json", "diarized_json", "bogus", "TEXT"} {
			body, ctype := uploadWith(t, formPart{name: "response_format", data: []byte(f)})
			resp, raw := post(t, srv.URL+"/v1/audio/transcriptions", ctype, body, true)
			if resp.StatusCode != 400 {
				t.Errorf("%s: status %d", f, resp.StatusCode)
				continue
			}
			if m := errMessage(raw); !strings.Contains(m, "json") || !strings.Contains(m, "text") {
				t.Errorf("%s: message should list json and text: %q", f, m)
			}
			if got := lastOutcome(t, cfg.Audit.Dir, i+1); got != "unsupported_format" {
				t.Errorf("%s: outcome %q", f, got)
			}
		}
		if back.count() != 0 {
			t.Errorf("refused formats reached the backend")
		}
	})

	t.Run("text with a backend error passes through", func(t *testing.T) {
		const reply = `{"error":{"message":"boom","type":"server_error"}}`
		back := jsonBackend(t, 500, "application/json", reply)
		srv := router(t, transcribeCfg(back))
		body, ctype := uploadWith(t, formPart{name: "response_format", data: []byte("text")})
		resp, raw := post(t, srv.URL+"/v1/audio/transcriptions", ctype, body, true)
		if resp.StatusCode != 500 || string(raw) != reply ||
			!strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			t.Errorf("error not passed through: %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), raw)
		}
	})

	t.Run("text with an unparseable or textless reply passes through", func(t *testing.T) {
		for _, reply := range []string{`not json`, `{"language":"en"}`, `{"text":42}`, `{"text":null}`, `[]`} {
			back := jsonBackend(t, 200, "application/json", reply)
			srv := router(t, transcribeCfg(back))
			body, ctype := uploadWith(t, formPart{name: "response_format", data: []byte("text")})
			resp, raw := post(t, srv.URL+"/v1/audio/transcriptions", ctype, body, true)
			if resp.StatusCode != 200 || string(raw) != reply ||
				!strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
				t.Errorf("%q not passed through: %d %q %q", reply, resp.StatusCode, resp.Header.Get("Content-Type"), raw)
			}
		}
	})

	t.Run("empty text is still text", func(t *testing.T) {
		back := jsonBackend(t, 200, "application/json", `{"text":""}`)
		srv := router(t, transcribeCfg(back))
		body, ctype := uploadWith(t, formPart{name: "response_format", data: []byte("text")})
		resp, raw := post(t, srv.URL+"/v1/audio/transcriptions", ctype, body, true)
		if resp.StatusCode != 200 || len(raw) != 0 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
			t.Errorf("%d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), raw)
		}
	})

	t.Run("text with stream=true passes the stream through", func(t *testing.T) {
		const events = "data: {\"type\":\"transcript.text.delta\",\"delta\":\"hi\"}\n\n" +
			"data: {\"type\":\"transcript.text.done\",\"text\":\"hi\"}\n\n"
		back := jsonBackend(t, 200, "text/event-stream", events)
		srv := router(t, transcribeCfg(back))
		body, ctype := uploadWith(t,
			formPart{name: "response_format", data: []byte("text")},
			formPart{name: "stream", data: []byte("true")})
		resp, raw := post(t, srv.URL+"/v1/audio/transcriptions", ctype, body, true)
		if resp.StatusCode != 200 || string(raw) != events ||
			!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Errorf("%d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), raw)
		}
	})
}

// --- streaming -------------------------------------------------------------

func TestTranscriptionStreamIsNotHeldBack(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	back := newStub(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"transcript.text.delta\",\"delta\":\"Hel\"}\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: {\"type\":\"transcript.text.done\",\"text\":\"Hello\"}\n\n")
	})
	// The substitution guard is on (the default) and the tier's backend name
	// differs from its tier name: both would act on a chat tier.
	srv := router(t, transcribeCfg(back))
	body, ctype := uploadWith(t, formPart{name: "stream", data: []byte("true")})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/audio/transcriptions", bytes.NewReader(body))
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}

	first := make(chan string, 1)
	br := bufio.NewReader(resp.Body)
	go func() {
		line, _ := br.ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if !strings.Contains(line, "transcript.text.delta") {
			t.Errorf("first event: %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first event did not arrive while the backend was still streaming")
	}
	close(release)
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "transcript.text.done") {
		t.Errorf("rest of stream: %q", rest)
	}
}

// --- audit -----------------------------------------------------------------

func TestAuditMultipart(t *testing.T) {
	audio := fakeAudio(64 * 1024)
	run := func(t *testing.T, a *Audit) (rec map[string]any, line string) {
		back := echoBackend(t)
		cfg := transcribeCfg(back)
		cfg.Audit = a
		srv := router(t, cfg)
		body, ctype := buildMultipart(t, testBoundary, []formPart{
			{name: "file", filename: "clip.wav", ctype: "audio/wav", data: audio},
			{name: "model", data: []byte("transcribe-great")},
			{name: "language", data: []byte("fr")},
			{name: "prompt", data: []byte(strings.Repeat("p", 500))},
		})
		resp, raw := post(t, srv.URL+"/v1/audio/transcriptions", ctype, body, true)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		recs := auditRecords(t, a.Dir, 1)
		files, _ := filepath.Glob(filepath.Join(a.Dir, "audit-*.jsonl"))
		data, _ := os.ReadFile(files[0])
		return recs[0], string(data)
	}

	t.Run("bodies on", func(t *testing.T) {
		max := 100
		rec, line := run(t, &Audit{Dir: t.TempDir(), MaxBodyBytes: max})
		if rec["outcome"] != "ok" || rec["tier"] != "transcribe-great" {
			t.Errorf("record: %v", rec)
		}
		files, _ := rec["files"].([]any)
		if len(files) != 1 {
			t.Fatalf("files: %v", rec["files"])
		}
		f := files[0].(map[string]any)
		if f["field"] != "file" || f["filename"] != "clip.wav" || f["content_type"] != "audio/wav" ||
			f["bytes"] != float64(len(audio)) || f["sha256"] != sum(audio) {
			t.Errorf("file metadata: %v", f)
		}
		req, _ := rec["request"].(map[string]any)
		if req["model"] != "transcribe-great" || req["language"] != "fr" {
			t.Errorf("request fields: %v", req)
		}
		if p, _ := req["prompt"].(string); len(p) != max {
			t.Errorf("prompt should be cut to %d bytes, is %d", max, len(p))
		}
		if _, ok := req["file"]; ok {
			t.Errorf("file part leaked into request: %v", req)
		}
		if rec["req_bytes"] == nil {
			t.Error("req_bytes missing")
		}
		if strings.Contains(line, "AUDIOMARKER") || len(line) > 4096 {
			t.Errorf("audio content reached the log (%d bytes)", len(line))
		}
	})

	t.Run("bodies off still records files", func(t *testing.T) {
		off := false
		rec, _ := run(t, &Audit{Dir: t.TempDir(), LogBodies: &off})
		if _, ok := rec["request"]; ok {
			t.Errorf("request recorded with log_bodies=false: %v", rec["request"])
		}
		if files, _ := rec["files"].([]any); len(files) != 1 {
			t.Errorf("files: %v", rec["files"])
		}
	})
}

func TestAuditKeepsNonJSONBodies(t *testing.T) {
	back := newStub(t, func(w http.ResponseWriter, r *http.Request, body []byte) {})
	cfg := &Config{Audit: &Audit{Dir: t.TempDir()}, Tiers: map[string]Tier{
		"great": {BaseURL: back.URL + "/v1", Model: "g"}}}
	srv := router(t, cfg)

	// Small, so it is under max_body_bytes and used to be stored as RawMessage,
	// which made the whole record fail to marshal and vanish.
	resp, _ := post(t, srv.URL+"/v1/chat/completions", "text/plain", []byte("this is not json {"), true)
	if resp.StatusCode != 400 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	rec := auditRecords(t, cfg.Audit.Dir, 1)[0]
	if rec["outcome"] != "bad_json" || rec["request"] != "this is not json {" {
		t.Errorf("record: %v", rec)
	}

	// Valid JSON is still stored structured, not as an escaped string.
	post(t, srv.URL+"/v1/chat/completions", "application/json", []byte(`{"model":"nope","x":[1,2]}`), true)
	rec = auditRecords(t, cfg.Audit.Dir, 2)[1]
	if _, ok := rec["request"].(map[string]any); !ok {
		t.Errorf("JSON body should stay structured: %#v", rec["request"])
	}

	// Truncation still applies, with its marker.
	cfg.Audit.MaxBodyBytes = 8
	post(t, srv.URL+"/v1/chat/completions", "text/plain", []byte("0123456789abcdef"), true)
	rec = auditRecords(t, cfg.Audit.Dir, 3)[2]
	if rec["request"] != "01234567" || rec["request_truncated"] != true {
		t.Errorf("truncation: %v", rec)
	}
}

// --- chat regression -------------------------------------------------------

func TestChatRewriteAndUnleak(t *testing.T) {
	back := newStub(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"1","model":"backend-model","system_fingerprint":"backend-model",`+
			`"choices":[{"message":{"content":"I am backend-model"}}]}`)
	})
	srv := router(t, &Config{Tiers: map[string]Tier{
		"fast": {BaseURL: back.URL + "/v1", Model: "backend-model"}}})

	resp, raw := post(t, srv.URL+"/v1/chat/completions", "application/json",
		[]byte(`{"model":"fast","temperature":0.10,"messages":[{"role":"user","content":"hi"}]}`), true)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	rq := back.request()
	if rq.Path != "/v1/chat/completions" || rq.ContentType != "application/json" {
		t.Errorf("backend saw %q %q", rq.Path, rq.ContentType)
	}
	var sent map[string]json.RawMessage
	json.Unmarshal(rq.Body, &sent)
	if string(sent["model"]) != `"backend-model"` || string(sent["temperature"]) != "0.10" {
		t.Errorf("request not rewritten as expected: %s", rq.Body)
	}
	if rq.Length != int64(len(rq.Body)) {
		t.Errorf("Content-Length %d, body %d", rq.Length, len(rq.Body))
	}

	var got struct {
		Model             string `json:"model"`
		SystemFingerprint string `json:"system_fingerprint"`
		Choices           []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	json.Unmarshal(raw, &got)
	if got.Model != "fast" || got.SystemFingerprint != "fast" {
		t.Errorf("backend name leaked: %s", raw)
	}
	// Metadata only: generated text is never altered.
	if got.Choices[0].Message.Content != "I am backend-model" {
		t.Errorf("content altered: %s", raw)
	}
	if resp.ContentLength != int64(len(raw)) {
		t.Errorf("Content-Length %d, body %d", resp.ContentLength, len(raw))
	}
}

func TestChatStreamUnleak(t *testing.T) {
	back := newStub(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"model\":\"keepalive\",\"created\":0}\n\n")
		io.WriteString(w, "data: {\"model\":\"backend-model\",\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	srv := router(t, &Config{Tiers: map[string]Tier{
		"fast": {BaseURL: back.URL + "/v1", Model: "backend-model"}}})
	resp, raw := post(t, srv.URL+"/v1/chat/completions", "application/json",
		[]byte(`{"model":"fast","stream":true}`), true)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), "backend-model") || strings.Contains(string(raw), "keepalive") {
		t.Errorf("stream leaked a backend name: %s", raw)
	}
	if !strings.Contains(string(raw), `"model":"fast"`) || !strings.Contains(string(raw), "data: [DONE]") {
		t.Errorf("stream: %s", raw)
	}
}

func TestChatModelSubstituted(t *testing.T) {
	t.Run("non-stream", func(t *testing.T) {
		back := newStub(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"model":"some-other-model"}`)
		})
		cfg := &Config{Audit: &Audit{Dir: t.TempDir()}, Tiers: map[string]Tier{
			"fast": {BaseURL: back.URL + "/v1", Model: "backend-model"}}}
		srv := router(t, cfg)
		resp, raw := post(t, srv.URL+"/v1/chat/completions", "application/json", []byte(`{"model":"fast"}`), true)
		if resp.StatusCode != 502 || !strings.Contains(errMessage(raw), "some-other-model") {
			t.Errorf("%d %s", resp.StatusCode, raw)
		}
		if got := lastOutcome(t, cfg.Audit.Dir, 1); got != "model_substituted" {
			t.Errorf("outcome %q", got)
		}
	})
	t.Run("stream", func(t *testing.T) {
		back := newStub(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"model\":\"keepalive\"}\n\n")
			io.WriteString(w, "data: {\"model\":\"some-other-model\"}\n\n")
		})
		cfg := &Config{Audit: &Audit{Dir: t.TempDir()}, Tiers: map[string]Tier{
			"fast": {BaseURL: back.URL + "/v1", Model: "backend-model"}}}
		srv := router(t, cfg)
		resp, raw := post(t, srv.URL+"/v1/chat/completions", "application/json",
			[]byte(`{"model":"fast","stream":true}`), true)
		if resp.StatusCode != 502 || strings.Contains(string(raw), "data:") {
			t.Errorf("%d %s", resp.StatusCode, raw)
		}
		if got := lastOutcome(t, cfg.Audit.Dir, 1); got != "model_substituted" {
			t.Errorf("outcome %q", got)
		}
	})
	t.Run("guard off", func(t *testing.T) {
		back := newStub(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"model":"some-other-model"}`)
		})
		off := false
		srv := router(t, &Config{VerifyResponseModel: &off, Tiers: map[string]Tier{
			"fast": {BaseURL: back.URL + "/v1", Model: "backend-model"}}})
		resp, _ := post(t, srv.URL+"/v1/chat/completions", "application/json", []byte(`{"model":"fast"}`), true)
		if resp.StatusCode != 200 {
			t.Errorf("status %d", resp.StatusCode)
		}
	})
}

func TestAuthRequired(t *testing.T) {
	back := newStub(t, func(w http.ResponseWriter, r *http.Request, body []byte) {})
	cfg := &Config{Audit: &Audit{Dir: t.TempDir()}, Tiers: map[string]Tier{
		"fast": {BaseURL: back.URL + "/v1", Model: "m"}}}
	srv := router(t, cfg)
	resp, raw := post(t, srv.URL+"/v1/chat/completions", "application/json", []byte(`{"model":"fast"}`), false)
	if resp.StatusCode != 401 {
		t.Errorf("status %d: %s", resp.StatusCode, raw)
	}
	if got := lastOutcome(t, cfg.Audit.Dir, 1); got != "denied" {
		t.Errorf("outcome %q", got)
	}
	// An upload is refused before its body matters.
	body, ctype := uploadWith(t)
	resp, _ = post(t, srv.URL+"/v1/audio/transcriptions", ctype, body, false)
	if resp.StatusCode != 401 {
		t.Errorf("multipart without a key: status %d", resp.StatusCode)
	}
	if back.count() != 0 {
		t.Errorf("unauthenticated request reached the backend")
	}
}

func TestBackendUnreachable(t *testing.T) {
	// Grab a port, then close it so nothing listens there.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + l.Addr().String() + "/v1"
	l.Close()

	cfg := &Config{Audit: &Audit{Dir: t.TempDir()}, Tiers: map[string]Tier{
		"fast":             {BaseURL: dead, Model: "m"},
		"transcribe-great": {BaseURL: dead, Model: "w", Kind: "transcription"},
	}}
	srv := router(t, cfg)

	resp, raw := post(t, srv.URL+"/v1/chat/completions", "application/json", []byte(`{"model":"fast"}`), true)
	if resp.StatusCode != 502 || !strings.Contains(errMessage(raw), `"fast"`) {
		t.Errorf("%d %s", resp.StatusCode, raw)
	}
	if got := lastOutcome(t, cfg.Audit.Dir, 1); got != "backend_unreachable" {
		t.Errorf("outcome %q", got)
	}

	body, ctype := uploadWith(t)
	resp, raw = post(t, srv.URL+"/v1/audio/transcriptions", ctype, body, true)
	if resp.StatusCode != 502 || !strings.Contains(errMessage(raw), `"transcribe-great"`) {
		t.Errorf("%d %s", resp.StatusCode, raw)
	}
	if got := lastOutcome(t, cfg.Audit.Dir, 2); got != "backend_unreachable" {
		t.Errorf("outcome %q", got)
	}
}
