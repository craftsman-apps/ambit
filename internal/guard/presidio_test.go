package guard

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/craftsman-apps/ambit/internal/extmcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// fakeAnalyzer stands in for Presidio's analyzer: it reports every occurrence of each word in finds, with
// offsets in characters (as the real one does). It records every text it receives.
type fakeAnalyzer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	texts []string
	fail  func(text string) bool
}

func newFakeAnalyzer(t *testing.T, finds map[string]string) *fakeAnalyzer {
	f := &fakeAnalyzer{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Text, Language string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.texts = append(f.texts, in.Text)
		f.mu.Unlock()
		if f.fail != nil && f.fail(in.Text) {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		out := []map[string]any{}
		for word, label := range finds {
			for from := 0; ; {
				i := strings.Index(in.Text[from:], word)
				if i < 0 {
					break
				}
				start := utf8.RuneCountInString(in.Text[:from+i])
				out = append(out, map[string]any{"entity_type": label, "start": start, "end": start + utf8.RuneCountInString(word), "score": 0.9})
				from += i + len(word)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAnalyzer) calls() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.texts) }

func (f *fakeAnalyzer) longest() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, t := range f.texts {
		n = max(n, utf8.RuneCountInString(t))
	}
	return n
}

func TestPresidioFindingAcrossTwoStringsIsNotMerged(t *testing.T) {
	// The fake reports "Kim\n\nLee" as one PERSON, as a real analyzer could for two names in a row.
	f := newFakeAnalyzer(t, map[string]string{"Kim\n\nLee": "PERSON"})
	p := &Presidio{AnalyzerURL: f.srv.URL}
	out, n, err := p.MaskAll(t.Context(), []string{"call Kim", "Lee today"})
	if err != nil || out[0] != "call <PERSON>" || out[1] != "<PERSON> today" || n != 2 {
		t.Fatalf("%q %d %v", out, n, err)
	}
}

func TestPresidioFindingInTheOverlapIsReplacedOnce(t *testing.T) {
	f := newFakeAnalyzer(t, map[string]string{"minsu@example.com": "EMAIL_ADDRESS"})
	// Place the email so that it lies inside the 200-character overlap of pieces 1 and 2.
	at := presidioPieceChars - 100
	text := strings.Repeat("x", at) + "minsu@example.com" + strings.Repeat("y", 500)
	p := &Presidio{AnalyzerURL: f.srv.URL}
	out, n, err := p.MaskAll(t.Context(), []string{text})
	want := strings.Repeat("x", at) + "<EMAIL_ADDRESS>" + strings.Repeat("y", 500)
	if err != nil || out[0] != want || n != 1 {
		t.Fatalf("n=%d err=%v same=%v", n, err, out[0] == want)
	}
	if f.calls() != 2 {
		t.Fatalf("want 2 pieces, got %d calls", f.calls())
	}
}

func TestPresidioFindingCutByAPieceEdgeIsStillWhole(t *testing.T) {
	f := newFakeAnalyzer(t, map[string]string{"minsu@example.com": "EMAIL_ADDRESS"})
	// The email starts 5 characters before the end of piece 1, so piece 1 sees only a part of it.
	at := presidioPieceChars - 5
	text := strings.Repeat("x", at) + "minsu@example.com" + strings.Repeat("y", 500)
	out, _, err := (&Presidio{AnalyzerURL: f.srv.URL}).MaskAll(t.Context(), []string{text})
	if err != nil || strings.Contains(out[0], "minsu") || strings.Contains(out[0], "example") || strings.Count(out[0], "<EMAIL_ADDRESS>") != 1 {
		t.Fatalf("masked wrongly (err=%v): %q", err, out[0][at-3:at+30])
	}
}

func TestPresidioOffsetsCountCharactersNotBytes(t *testing.T) {
	f := newFakeAnalyzer(t, map[string]string{"김민수": "PERSON"})
	out, n, err := (&Presidio{AnalyzerURL: f.srv.URL}).MaskAll(t.Context(), []string{"고객 김민수 님", "안녕 김민수"})
	if err != nil || n != 2 || out[0] != "고객 <PERSON> 님" || out[1] != "안녕 <PERSON>" {
		t.Fatalf("%q %d %v", out, n, err)
	}
}

func TestPresidioOnePieceFailingWithholdsTheResult(t *testing.T) {
	f := newFakeAnalyzer(t, map[string]string{"minsu@example.com": "EMAIL_ADDRESS"})
	f.fail = func(text string) bool { return strings.Contains(text, "BAD") }
	text := strings.Repeat("x", 150_000) + "BAD" + strings.Repeat("y", 150_000)
	g := &Guard{Pins: pins, Masker: &Presidio{AnalyzerURL: f.srv.URL}}
	body, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": "minsu@example.com"}, map[string]any{"type": "text", "text": text}}})
	res, _ := g.CheckResponse(t.Context(), &extmcp.McpResponse{ServiceNames: []string{"crm"}, Method: "tools/call", McpResponse: body})
	if res.GetError() == nil || res.GetMutated() != nil || res.GetPass() != nil {
		t.Fatalf("want the result withheld: %v", res)
	}
}

func TestPresidioManySmallStringsNeedFewCalls(t *testing.T) {
	f := newFakeAnalyzer(t, map[string]string{"minsu@example.com": "EMAIL_ADDRESS"})
	texts := make([]string, 2000)
	for i := range texts {
		texts[i] = "row " + strings.Repeat("a", 40) + " minsu@example.com"
	}
	out, n, err := (&Presidio{AnalyzerURL: f.srv.URL}).MaskAll(t.Context(), texts)
	if err != nil || n != 2000 || strings.Contains(out[1999], "minsu") || !strings.HasSuffix(out[1999], "<EMAIL_ADDRESS>") {
		t.Fatalf("%d %v", n, err)
	}
	if c := f.calls(); c > 2 { // 2000 x ~65 characters is about 130,000 characters: 2 pieces
		t.Fatalf("%d analyze calls for 2000 strings", c)
	}
}

func TestPresidioOneHugeStringIsSplit(t *testing.T) {
	f := newFakeAnalyzer(t, map[string]string{"minsu@example.com": "EMAIL_ADDRESS"})
	text := strings.Repeat("a", 1_500_000)
	text = text[:700_000] + "minsu@example.com" + text[700_000:]
	out, n, err := (&Presidio{AnalyzerURL: f.srv.URL}).MaskAll(t.Context(), []string{text})
	if err != nil || n != 1 || len(out[0]) != 1_500_000+len("<EMAIL_ADDRESS>") {
		t.Fatalf("n=%d err=%v len=%d", n, err, len(out[0]))
	}
	if l := f.longest(); l > presidioPieceChars {
		t.Fatalf("an analyze call had %d characters", l)
	}
	if f.calls() < 15 {
		t.Fatalf("only %d pieces", f.calls())
	}
}

func TestPresidioEmptyResultMakesNoCall(t *testing.T) {
	f := newFakeAnalyzer(t, nil)
	p := &Presidio{AnalyzerURL: f.srv.URL}
	for _, in := range [][]string{nil, {}, {""}, {" ", "\n\t"}} {
		out, n, err := p.MaskAll(t.Context(), in)
		if err != nil || n != 0 || len(out) != len(in) {
			t.Fatalf("%q: %q %d %v", in, out, n, err)
		}
	}
	if f.calls() != 0 {
		t.Fatalf("%d calls", f.calls())
	}
}

func TestPresidioWholeDeadlineApplies(t *testing.T) {
	var gate atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // the server notices a closed connection only after the body is read
		gate.Store(true)
		<-r.Context().Done()
	}))
	defer srv.Close()
	p := &Presidio{AnalyzerURL: srv.URL, Timeout: 100 * time.Millisecond}
	if _, _, err := p.MaskAll(t.Context(), []string{"Kim called"}); err == nil || !gate.Load() {
		t.Fatalf("want a deadline error, got %v", err)
	}
}

// A tool result of 6 MiB (above grpc-go's default 4 MiB limit) passes through the guard's gRPC server and
// comes back masked.
func TestGuardServesAToolResultOverFourMiB(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(ServerOptions()...)
	extmcp.RegisterExtMcpServer(srv, &Guard{Pins: pins, Masker: Builtin{}})
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessage), grpc.MaxCallSendMsgSize(maxMessage)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	text := strings.Repeat("lorem ipsum ", 550_000) + " minsu.kim@example.com"
	body, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}})
	if len(body) < 6<<20 {
		t.Fatalf("test body is only %d bytes", len(body))
	}
	res, err := extmcp.NewExtMcpClient(conn).CheckResponse(t.Context(), &extmcp.McpResponse{ServiceNames: []string{"crm"}, Method: "tools/call", McpResponse: body})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.GetMutated()); !strings.Contains(got, "<EMAIL>") || strings.Contains(got, "minsu.kim") {
		t.Fatalf("not masked: %v", res)
	}
}

const customerRecord = "Kim Minsu: resident registration number 900101-1234567, card 4111 1111 1111 1111, mail minsu@example.com"

func TestBuiltinMasksStillRunWhenPresidioIsOn(t *testing.T) {
	f := newFakeAnalyzer(t, nil) // Presidio finds nothing, as it may for a Korean resident registration number
	m := BuiltinThenPresidio{Presidio: &Presidio{AnalyzerURL: f.srv.URL}}
	out, n, err := m.Mask(t.Context(), customerRecord)
	if err != nil || n != 3 || strings.Contains(out, "900101") || strings.Contains(out, "4111") ||
		!strings.Contains(out, "<NATIONAL_ID>") || !strings.Contains(out, "<CARD_NUMBER>") {
		t.Fatalf("%q %d %v", out, n, err)
	}
}

func TestBuiltinAndPresidioCountsAddUpAndPlaceholdersSurvive(t *testing.T) {
	// Presidio finds a name, and also reports the word inside the built-in placeholder as a name.
	f := newFakeAnalyzer(t, map[string]string{"Kim Minsu": "PERSON", "NATIONAL_ID": "PERSON"})
	m := BuiltinThenPresidio{Presidio: &Presidio{AnalyzerURL: f.srv.URL}}
	out, n, err := m.MaskAll(t.Context(), []string{customerRecord, "call Kim Minsu"})
	want0 := "<PERSON>: resident registration number <NATIONAL_ID>, card <CARD_NUMBER>, mail <EMAIL>"
	if err != nil || n != 5 || out[0] != want0 || out[1] != "call <PERSON>" {
		t.Fatalf("%q %d %v", out, n, err)
	}
}

func TestPresidioStillChecksOtherTextInAngleBrackets(t *testing.T) {
	f := newFakeAnalyzer(t, map[string]string{"JOHN": "PERSON"})
	m := BuiltinThenPresidio{Presidio: &Presidio{AnalyzerURL: f.srv.URL}}
	if out, _, err := m.Mask(t.Context(), "<JOHN> wrote"); err != nil || out != "<<PERSON>> wrote" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestBuiltinThenPresidioFailsClosed(t *testing.T) {
	f := newFakeAnalyzer(t, nil)
	f.fail = func(string) bool { return true }
	g := &Guard{Pins: pins, Masker: BuiltinThenPresidio{Presidio: &Presidio{AnalyzerURL: f.srv.URL}}}
	res, _ := g.CheckResponse(t.Context(), &extmcp.McpResponse{ServiceNames: []string{"crm"}, Method: "tools/call",
		McpResponse: []byte(`{"content":[{"type":"text","text":"900101-1234567"}]}`)})
	if res.GetError() == nil || res.GetMutated() != nil || res.GetPass() != nil {
		t.Fatalf("want the result withheld: %v", res)
	}
}
