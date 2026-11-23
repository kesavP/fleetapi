package costs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var bg = context.Background()

type item struct {
	RetailPrice   float64 `json:"retailPrice"`
	UnitPrice     float64 `json:"unitPrice"`
	ArmSkuName    string  `json:"armSkuName"`
	SkuName       string  `json:"skuName"`
	ProductName   string  `json:"productName"`
	UnitOfMeasure string  `json:"unitOfMeasure"`
	Type          string  `json:"type"`
}

// azStub is Azure's retail prices API as far as it matters here.
type azStub struct {
	srv     *httptest.Server
	mu      sync.Mutex
	queries []string
	headers []http.Header
	pages   [][]item // answered in order, the next page by the link
	status  int
}

func newAzStub(t *testing.T, pages ...[]item) *azStub {
	t.Helper()
	s := &azStub{pages: pages, status: http.StatusOK}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.queries = append(s.queries, r.URL.RawQuery)
		s.headers = append(s.headers, r.Header.Clone())
		page := 0
		if p := r.URL.Query().Get("page"); p != "" {
			page = int(p[0] - '0')
		}
		st := s.status
		var body map[string]any
		if page < len(s.pages) {
			body = map[string]any{"Items": s.pages[page]}
			if page+1 < len(s.pages) {
				body["NextPageLink"] = "http://" + r.Host + r.URL.Path + "?page=" + string(rune('0'+page+1))
			}
		} else {
			body = map[string]any{"Items": []item{}}
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *azStub) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.queries) }

func vm(price float64, over func(*item)) item {
	i := item{RetailPrice: price, UnitPrice: price, ArmSkuName: "Standard_D4s_v6", SkuName: "D4s v6", ProductName: "Virtual Machines Dsv6 Series", UnitOfMeasure: "1 Hour", Type: "Consumption"}
	if over != nil {
		over(&i)
	}
	return i
}

func newAzure(s *azStub, clock *time.Time) *AzureRetail {
	return &AzureRetail{BaseURL: s.srv.URL, Now: func() time.Time { return *clock }}
}

func TestTheLinuxOnDemandPriceOfAnInstanceTypeIsReadFromAzure(t *testing.T) {
	s := newAzStub(t, []item{
		vm(0.04, func(i *item) { i.ProductName = "Virtual Machines Dsv6 Series Windows" }),
		vm(0.05, func(i *item) { i.SkuName = "D4s v6 Spot" }),
		vm(0.06, func(i *item) { i.SkuName = "D4s v6 Low Priority" }),
		vm(0.30, func(i *item) { i.UnitOfMeasure = "1 Month" }),
		vm(0.19, nil),
		vm(0.21, nil), // another meter of the same: the lowest is taken
		vm(0, nil),    // a price of nothing is not a price
	})
	now := time.Now()
	got, err := newAzure(s, &now).Hourly(bg, "southindia", "Standard_D4s_v6")
	if err != nil || got != 0.19 {
		t.Fatalf("price = %v, err %v", got, err)
	}
	q := s.queries[0]
	for _, want := range []string{"serviceName+eq+%27Virtual+Machines%27", "armRegionName+eq+%27southindia%27", "armSkuName+eq+%27Standard_D4s_v6%27", "priceType+eq+%27Consumption%27", "currencyCode=USD"} {
		if !strings.Contains(q, want) && !strings.Contains(strings.ReplaceAll(q, "%20", "+"), want) {
			t.Errorf("the query lacks %q: %s", want, q)
		}
	}
	if s.headers[0].Get("Authorization") != "" || s.headers[0].Get("Cookie") != "" {
		t.Fatalf("credentials were sent to a public API: %v", s.headers[0])
	}
}

func TestAnAnswerIsKeptForADayAndAFailureForTenMinutes(t *testing.T) {
	s := newAzStub(t, []item{vm(0.19, nil)})
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	az := newAzure(s, &now)
	for i := 0; i < 3; i++ {
		if _, err := az.Hourly(bg, "southindia", "Standard_D4s_v6"); err != nil {
			t.Fatal(err)
		}
	}
	if s.count() != 1 {
		t.Fatalf("asked %d times: the price of an instance type does not change by the minute", s.count())
	}
	now = now.Add(23 * time.Hour)
	az.Hourly(bg, "southindia", "Standard_D4s_v6")
	if s.count() != 1 {
		t.Fatal("asked again within a day")
	}
	now = now.Add(2 * time.Hour)
	az.Hourly(bg, "southindia", "Standard_D4s_v6")
	if s.count() != 2 {
		t.Fatalf("asked %d times after a day", s.count())
	}

	bad := newAzStub(t)
	bad.status = http.StatusInternalServerError
	now2 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	az2 := newAzure(bad, &now2)
	if _, err := az2.Hourly(bg, "southindia", "Standard_D4s_v6"); err == nil {
		t.Fatal("a failure was not an error")
	}
	az2.Hourly(bg, "southindia", "Standard_D4s_v6")
	if bad.count() != 1 {
		t.Fatalf("asked %d times: a failed lookup must not be repeated on every page view", bad.count())
	}
	now2 = now2.Add(11 * time.Minute)
	az2.Hourly(bg, "southindia", "Standard_D4s_v6")
	if bad.count() != 2 {
		t.Fatalf("asked %d times after ten minutes", bad.count())
	}
}

func TestAnInstanceTypeAzureHasNoPriceForIsAnErrorThatSaysSoAndIsKept(t *testing.T) {
	s := newAzStub(t, []item{vm(0.3, func(i *item) { i.SkuName = "D4s v6 Spot" })})
	now := time.Now()
	az := newAzure(s, &now)
	if _, err := az.Hourly(bg, "southindia", "Standard_D4s_v6"); !errors.Is(err, ErrNoPrice) {
		t.Fatalf("err = %v", err)
	}
	az.Hourly(bg, "southindia", "Standard_D4s_v6")
	if s.count() != 1 {
		t.Fatal("a missing price was asked for again at once")
	}
	now = now.Add(61 * time.Minute)
	az.Hourly(bg, "southindia", "Standard_D4s_v6")
	if s.count() != 2 {
		t.Fatalf("asked %d times after an hour: a price that was missing may have been added, and a day is too long to wait", s.count())
	}
}

func TestALinkToAnotherHostIsNotFollowed(t *testing.T) {
	var asked int
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { asked++ }))
	defer evil.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"Items": []item{}, "NextPageLink": evil.URL + "/steal"})
	}))
	defer s.Close()
	now := time.Now()
	if _, err := (&AzureRetail{BaseURL: s.URL, Now: func() time.Time { return now }}).Hourly(bg, "southindia", "Standard_D4s_v6"); err == nil {
		t.Fatal("a price list that sent us elsewhere gave a price")
	}
	if asked != 0 {
		t.Fatalf("the link to another host was followed %d times", asked)
	}
}

func TestARegionOrSizeThatCouldChangeTheQueryIsNotAskedAbout(t *testing.T) {
	s := newAzStub(t, []item{vm(0.19, nil)})
	now := time.Now()
	az := newAzure(s, &now)
	for _, c := range [][2]string{{"south india", "Standard_D4s_v6"}, {"southindia", "X' or '1' eq '1"}, {"southindia", ""}, {"", "Standard_D4s_v6"}, {"southindia", "a&b=c"}} {
		if _, err := az.Hourly(bg, c[0], c[1]); err == nil {
			t.Errorf("%q %q was asked about", c[0], c[1])
		}
	}
	if s.count() != 0 {
		t.Fatalf("%d requests were made for values that are not names", s.count())
	}
}

func TestAnAnswerInSeveralPagesIsReadToTheEnd(t *testing.T) {
	s := newAzStub(t, []item{vm(0.40, nil)}, []item{vm(0.19, nil)})
	now := time.Now()
	got, err := newAzure(s, &now).Hourly(bg, "southindia", "Standard_D4s_v6")
	if err != nil || got != 0.19 || s.count() != 2 {
		t.Fatalf("price %v err %v after %d requests", got, err, s.count())
	}
}

func TestAnAnswerThatIsNotJSONOrIsHugeIsAnErrorAndNotAPrice(t *testing.T) {
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>nope</html>")) }))
	defer junk.Close()
	now := time.Now()
	if _, err := (&AzureRetail{BaseURL: junk.URL, Now: func() time.Time { return now }}).Hourly(bg, "southindia", "Standard_D4s_v6"); err == nil {
		t.Fatal("a page that is not JSON gave a price")
	}
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"Items":[` + strings.Repeat(`{"retailPrice":1},`, 400000) + `{}]}`))
	}))
	defer huge.Close()
	if _, err := (&AzureRetail{BaseURL: huge.URL, Now: func() time.Time { return now }}).Hourly(bg, "southindia", "Standard_D4s_v6"); err == nil {
		t.Fatal("an answer of many megabytes was accepted")
	}
}

func TestAServerThatDoesNotAnswerInTimeIsAnError(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(2 * time.Second) }))
	defer slow.Close()
	now := time.Now()
	az := &AzureRetail{BaseURL: slow.URL, Timeout: 100 * time.Millisecond, Now: func() time.Time { return now }}
	start := time.Now()
	if _, err := az.Hourly(bg, "southindia", "Standard_D4s_v6"); err == nil || time.Since(start) > time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

// ---- the prices a node has ----

func TestAnOverrideBeatsTheCloudAndSpotHasItsOwnPrice(t *testing.T) {
	s := newAzStub(t, []item{vm(0.19, nil)})
	now := time.Now()
	p := &Prices{Overrides: map[string]Override{"standard_d4s_v6": {Hourly: 0.25, SpotHourly: 0.07}}, Azure: newAzure(s, &now)}
	got, src, err := p.Hourly(bg, "Azure", "SouthIndia", "Standard_D4s_v6", "on-demand")
	if err != nil || got == nil || *got != 0.25 || src != "override" {
		t.Fatalf("%v %q %v", got, src, err)
	}
	if got, src, _ = p.Hourly(bg, "Azure", "SouthIndia", "Standard_D4s_v6", "spot"); got == nil || *got != 0.07 || src != "override" {
		t.Fatalf("spot: %v %q", got, src)
	}
	if s.count() != 0 {
		t.Fatal("the cloud was asked about a type that has an override")
	}
	only := &Prices{Overrides: map[string]Override{"Standard_D4s_v6": {Hourly: 0.25}}}
	if got, _, _ = only.Hourly(bg, "Azure", "x", "Standard_D4s_v6", "spot"); got == nil || *got != 0.25 {
		t.Fatalf("a spot node whose override has no spot price takes the on-demand one: %v", got)
	}
}

func TestWithoutAnOverrideAnAzureNodeIsPricedFromAzureWithItsRegionLowerCased(t *testing.T) {
	s := newAzStub(t, []item{vm(0.19, nil)})
	now := time.Now()
	p := &Prices{Azure: newAzure(s, &now)}
	got, src, err := p.Hourly(bg, "Azure", "SouthIndia", "Standard_D4s_v6", "")
	if err != nil || got == nil || *got != 0.19 || src != "azure-retail" {
		t.Fatalf("%v %q %v", got, src, err)
	}
	if !strings.Contains(s.queries[0], "southindia") || strings.Contains(s.queries[0], "SouthIndia") {
		t.Fatalf("the region was not lower-cased: %s", s.queries[0])
	}
}

func TestNothingIsFoundForAnotherCloudOrWithAzureSwitchedOffAndThatIsNotAnError(t *testing.T) {
	now := time.Now()
	p := &Prices{Azure: newAzure(newAzStub(t, []item{vm(0.19, nil)}), &now)}
	if got, src, err := p.Hourly(bg, "AWS", "us-east-1", "m5.xlarge", ""); got != nil || src != "" || err != nil {
		t.Fatalf("aws: %v %q %v", got, src, err)
	}
	off := &Prices{}
	if got, _, err := off.Hourly(bg, "Azure", "southindia", "Standard_D4s_v6", ""); got != nil || err != nil {
		t.Fatalf("azure off: %v %v", got, err)
	}
}

func TestAFailureOfAzureIsReportedWithNoPrice(t *testing.T) {
	bad := newAzStub(t)
	bad.status = 503
	now := time.Now()
	p := &Prices{Azure: newAzure(bad, &now)}
	got, _, err := p.Hourly(bg, "Azure", "southindia", "Standard_D4s_v6", "")
	if got != nil || err == nil {
		t.Fatalf("%v %v: a lookup that failed must say so, so the page can", got, err)
	}
	none := newAzStub(t, []item{})
	p2 := &Prices{Azure: newAzure(none, &now)}
	if got, _, err = p2.Hourly(bg, "Azure", "southindia", "Standard_Nonexistent", ""); got != nil || !errors.Is(err, ErrNoPrice) {
		t.Fatalf("%v %v", got, err)
	}
}

func TestOverridesAreParsedAndCheckedAtStart(t *testing.T) {
	got, err := ParseOverrides(`{"Standard_D4s_v6":{"hourly":0.25,"spotHourly":0.07},"Standard_E8s_v5":{"hourly":0.5}}`)
	if err != nil || len(got) != 2 || got["Standard_D4s_v6"].SpotHourly != 0.07 || got["Standard_E8s_v5"].Hourly != 0.5 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, empty := range []string{"", "  ", "{}"} {
		if got, err := ParseOverrides(empty); err != nil || len(got) != 0 {
			t.Errorf("%q: %v %v", empty, got, err)
		}
	}
	for name, raw := range map[string]string{
		"not JSON": `{`, "an array": `[]`, "no price": `{"x":{}}`, "a negative price": `{"x":{"hourly":-1}}`, "a zero price": `{"x":{"hourly":0}}`,
		"a negative spot price": `{"x":{"hourly":1,"spotHourly":-1}}`, "an unknown setting": `{"x":{"hourly":1,"price":2}}`, "text for a price": `{"x":{"hourly":"1"}}`,
		"something after": `{} {}`, "an empty name": `{"":{"hourly":1}}`,
	} {
		if _, err := ParseOverrides(raw); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
