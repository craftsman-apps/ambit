package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Recall test for decision D7 (issue #204): does Presidio find as many names, emails, phone numbers and
// addresses in the columnar form of a JSON result as in the plain form? It needs a running Presidio
// analyzer, so it is skipped unless AMBIT_PRESIDIO_TEST_URL is set. Run it with `make test-presidio`.

// known is one personal value the fixtures contain, and the Presidio entity type that should cover it.
type known struct{ typ, value string }

var (
	westFirst = []string{"Olivia", "Liam", "Emma", "Noah", "Ava", "Ethan", "Sophia", "Mason", "Isabella", "Lucas", "Mia", "Henry", "Amelia", "Jack", "Harper", "Owen", "Evelyn", "Caleb", "Grace", "Isaac", "Chloe", "Nathan", "Zoe", "Dylan", "Hannah", "Leo", "Nora", "Adrian", "Claire", "Felix"}
	westLast  = []string{"Anderson", "Brooks", "Carter", "Dawson", "Ellis", "Foster", "Gibson", "Hughes", "Jenkins", "Keller", "Lawson", "Morgan", "Nguyen", "Owens", "Parker", "Quinn", "Reynolds", "Sullivan", "Turner", "Vaughn", "Walsh", "Young", "Bennett", "Coleman", "Hayes", "Murphy", "Porter", "Ramos", "Stevens", "Thornton"}
	korFamily = []string{"Kim", "Lee", "Park", "Choi", "Jung", "Kang", "Cho", "Yoon", "Jang", "Lim", "Han", "Oh", "Seo", "Shin", "Kwon", "Hwang", "Ahn", "Song", "Yoo", "Hong"}
	korGiven  = []string{"Min-jun", "Seo-yeon", "Ji-ho", "Ha-eun", "Do-yun", "Su-bin", "Jun-seo", "Ye-jin", "Hyun-woo", "Ji-woo", "Seung-min", "Da-eun", "Tae-yang", "So-yeon", "Joon-ho", "Eun-ji", "Dong-hyun", "Na-yeon", "Sang-woo", "Hye-jin"}
	usStreets = []string{"Maple Street", "Oak Avenue", "Cedar Lane", "Pine Road", "Elm Drive", "Lakeview Boulevard", "Sunset Way", "Highland Court", "Willow Street", "Park Avenue"}
	usCities  = []string{"Portland, OR 97205", "Austin, TX 78701", "Denver, CO 80202", "Boston, MA 02108", "Seattle, WA 98101", "Chicago, IL 60601", "Atlanta, GA 30303", "Madison, WI 53703"}
	korRoads  = []string{"Teheran-ro", "Sejong-daero", "Hakdong-ro", "Gangnam-daero", "Yeongdong-daero", "Jong-ro", "Olympic-ro", "Mapo-daero"}
	korAreas  = []string{"Gangnam-gu, Seoul", "Jongno-gu, Seoul", "Mapo-gu, Seoul", "Songpa-gu, Seoul", "Haeundae-gu, Busan", "Suseong-gu, Daegu", "Yeonsu-gu, Incheon", "Seo-gu, Gwangju"}
	subjects  = []string{"Cannot log in after password reset", "Invoice shows the wrong amount", "Export to CSV is empty", "Request to change the billing contact", "Webhook returns status 500", "Dashboard loads slowly", "Seat count does not match the plan", "Two-factor codes arrive late"}
	companies = []string{"Northwind Traders", "Contoso Labs", "Harbor Freight Logistics", "Blue Mesa Analytics", "Kestrel Robotics", "Hanbit Software", "Daon Materials", "Mirae Foods"}
	titles    = []string{"Account Manager", "Engineer", "Procurement Lead", "Operations Director", "Support Analyst", "Finance Controller"}
)

// fixtureSet builds n records of each of the three tool-result kinds with unique personal values. The same seed
// gives the same fixtures. Every document is one JSON array of 30 objects with the same keys, so the columnar
// form applies; each document stays far below one Presidio piece (100,000 characters).
func fixtureSet(n int) (docs []string, truth [][]known) {
	r := rand.New(rand.NewSource(204))
	used := map[string]bool{}
	person := func() (name, local string, korean bool) {
		for {
			if r.Intn(3) == 0 { // one in three is a Korean romanized name
				f, g := korFamily[r.Intn(len(korFamily))], korGiven[r.Intn(len(korGiven))]
				name, local, korean = f+" "+g, strings.ToLower(f+"."+strings.ReplaceAll(g, "-", "")), true
			} else {
				f, l := westFirst[r.Intn(len(westFirst))], westLast[r.Intn(len(westLast))]
				name, local, korean = f+" "+l, strings.ToLower(f+"."+l), false
			}
			if !used[name] {
				used[name] = true
				return
			}
		}
	}
	phone := func(korean bool) string {
		if korean {
			return fmt.Sprintf("+82 10-%04d-%04d", 1000+r.Intn(9000), r.Intn(10000))
		}
		return fmt.Sprintf("(%d) %03d-%04d", 201+r.Intn(780), 200+r.Intn(800), r.Intn(10000))
	}
	address := func(korean bool) string {
		if korean {
			return fmt.Sprintf("%d %s, %s", 1+r.Intn(400), korRoads[r.Intn(len(korRoads))], korAreas[r.Intn(len(korAreas))])
		}
		return fmt.Sprintf("%d %s, %s", 10+r.Intn(9000), usStreets[r.Intn(len(usStreets))], usCities[r.Intn(len(usCities))])
	}
	const perDoc = 30
	for kind := 0; kind < 3; kind++ {
		for made := 0; made < n; made += perDoc {
			var rows []map[string]any
			var tr []known
			for i := 0; i < perDoc; i++ {
				name, local, kor := person()
				domain := []string{"example.com", "example.org", "mail.example.net"}[r.Intn(3)]
				email := fmt.Sprintf("%s%d@%s", local, r.Intn(90)+10, domain)
				ph, addr := phone(kor), address(kor)
				tr = append(tr, known{"PERSON", name}, known{"EMAIL_ADDRESS", email}, known{"PHONE_NUMBER", ph}, known{"LOCATION", addr})
				switch kind {
				case 0: // CRM contacts
					rows = append(rows, map[string]any{"id": fmt.Sprintf("c-%05d", made+i), "name": name, "email": email, "phone": ph,
						"address": addr, "company": companies[r.Intn(len(companies))], "title": titles[r.Intn(len(titles))], "created": "2026-03-14T09:21:00Z", "active": r.Intn(2) == 0})
				case 1: // support tickets
					rows = append(rows, map[string]any{"id": fmt.Sprintf("T-%06d", made+i), "subject": subjects[r.Intn(len(subjects))], "status": []string{"open", "pending", "solved"}[r.Intn(3)],
						"priority": []string{"low", "normal", "high"}[r.Intn(3)], "requester_name": name, "requester_email": email, "requester_phone": ph,
						"requester_address": addr, "updated": "2026-04-02T16:40:00Z"})
				default: // user directory
					rows = append(rows, map[string]any{"user_id": 9000 + made + i, "username": local, "full_name": name, "email": email, "mobile": ph,
						"location": addr, "role": []string{"admin", "member", "viewer"}[r.Intn(3)], "last_login": "2026-05-20T07:05:00Z", "mfa": r.Intn(2) == 0})
				}
			}
			b, _ := json.MarshalIndent(rows, "", "  ")
			docs, truth = append(docs, string(b)), append(truth, tr)
		}
	}
	return docs, truth
}

// recordingClient keeps the text and the findings of every analyzer call, so the test can see which known
// value Presidio covered, with the same offsets the guard uses to replace it.
type recordingClient struct {
	mu    sync.Mutex
	texts []string
	finds [][]finding
}

func (c *recordingClient) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	req.Body = io.NopCloser(bytes.NewReader(body))
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var in struct{ Text string }
	var fs []finding
	_ = json.Unmarshal(body, &in)
	_ = json.Unmarshal(data, &fs)
	c.mu.Lock()
	c.texts, c.finds = append(c.texts, in.Text), append(c.finds, fs)
	c.mu.Unlock()
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, nil
}

// covered reports whether Presidio marked at least half of the characters of value, with the wanted type.
// A value that is not in the text any more was masked before Presidio ran (the built-in masks do emails).
func covered(text string, fs []finding, k known) (ok, builtin bool) {
	i := strings.Index(text, k.value)
	if i < 0 {
		return true, true
	}
	start := len([]rune(text[:i]))
	end := start + len([]rune(k.value))
	hit := 0
	seen := map[int]bool{}
	for _, f := range fs {
		if f.Type != k.typ {
			continue
		}
		for p := max(f.Start, start); p < min(f.End, end); p++ {
			if !seen[p] {
				seen[p], hit = true, hit+1
			}
		}
	}
	return hit*2 >= end-start, false
}

type forms struct {
	name  string
	apply func(string) string
}

func TestPresidioRecallColumnarVersusPlain(t *testing.T) {
	url := os.Getenv("AMBIT_PRESIDIO_TEST_URL")
	if url == "" {
		t.Skip("AMBIT_PRESIDIO_TEST_URL is not set (run `make test-presidio`)")
	}
	docs, truth := fixtureSet(300)
	formList := []forms{
		{"original (pretty)", func(s string) string { return s }},
		{"plain (minified)", func(s string) string { o, ok := compressJSON(s, false); must(t, ok); return o }},
		{"columnar", func(s string) string {
			o, ok := compressJSON(s, true)
			must(t, ok)
			if !strings.Contains(o, `"columns"`) {
				t.Fatalf("fixture did not become columnar: %.200s", o)
			}
			return o
		}},
	}
	types := []string{"PERSON", "EMAIL_ADDRESS", "PHONE_NUMBER", "LOCATION"}
	recall := map[string]map[string][2]int{} // form -> type -> {found, total}
	for _, f := range formList {
		rc := &recordingClient{}
		recall[f.name] = map[string][2]int{}
		for i, d := range docs {
			text := f.apply(d)
			rc.texts, rc.finds = nil, nil
			// The guard path: built-in masks first, then Presidio. The recording transport sees the one call.
			guard := BuiltinThenPresidio{Presidio: &Presidio{AnalyzerURL: url, Timeout: 5 * time.Minute, HTTP: &http.Client{Transport: rc}}}
			if _, _, err := guard.Mask(context.Background(), text); err != nil {
				t.Fatalf("%s doc %d: %v", f.name, i, err)
			}
			if len(rc.texts) != 1 {
				t.Fatalf("%s doc %d: want 1 analyzer call, got %d", f.name, i, len(rc.texts))
			}
			for _, k := range truth[i] {
				ok, _ := covered(rc.texts[0], rc.finds[0], k)
				c := recall[f.name][k.typ]
				c[1]++
				if ok {
					c[0]++
				}
				recall[f.name][k.typ] = c
			}
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "\nPresidio recall through the guard path (Builtin, then Presidio), %d documents\n", len(docs))
	fmt.Fprintf(&out, "%-15s %-6s", "entity", "n")
	for _, f := range formList {
		fmt.Fprintf(&out, " %18s", f.name)
	}
	fmt.Fprintf(&out, " %12s\n", "col - plain")
	worst := 0.0
	for _, ty := range types {
		fmt.Fprintf(&out, "%-15s %-6d", ty, recall[formList[0].name][ty][1])
		pct := map[string]float64{}
		for _, f := range formList {
			c := recall[f.name][ty]
			pct[f.name] = 100 * float64(c[0]) / float64(c[1])
			fmt.Fprintf(&out, " %17.1f%%", pct[f.name])
		}
		drop := pct["plain (minified)"] - pct["columnar"]
		worst = max(worst, drop)
		fmt.Fprintf(&out, " %+11.1f\n", -drop)
		// Sanity: if Presidio finds almost nothing in the original text, the setup is wrong and the
		// comparison means nothing (the analyzer needs its English model; addresses are hard for it).
		if ty != "LOCATION" && pct["original (pretty)"] < 80 {
			t.Errorf("%s recall in the original text is %.1f%%: is the analyzer set up correctly?", ty, pct["original (pretty)"])
		}
	}
	if worst > 1.0 {
		fmt.Fprintf(&out, "Verdict: columnar lowers recall by up to %.1f points (limit 1): keep the restriction (D7).\n", worst)
	} else {
		fmt.Fprintf(&out, "Verdict: columnar is within 1 point for every type: the restriction (D7) can be removed.\n")
	}
	t.Log(out.String())
}

func must(t *testing.T, ok bool) {
	t.Helper()
	if !ok {
		t.Fatal("fixture is not valid JSON for the compressor")
	}
}
