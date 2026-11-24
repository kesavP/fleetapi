package costs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Prices say what a node costs per hour. In order: a price the operator set for its instance type (an override, which is also how spot nodes are priced, as
// the cloud does not say which they are), and for Azure the public list price of the type in its region (the Azure Retail Prices API: no credentials, and
// the hub only asks about types its own nodes have). Another cloud has no price here until an override gives one, and a node with no price is shown but is
// left out of every figure. These are list prices: an enterprise agreement, reservations or savings plans make the real ones lower.

// ErrNoPrice means the price list has no price for the instance type in the region.
var ErrNoPrice = errors.New("no price in the list")

// Override is a price the operator set for an instance type: per hour on-demand, and optionally for a spot node.
type Override struct {
	Hourly     float64 `json:"hourly"`
	SpotHourly float64 `json:"spotHourly"`
}

// ParseOverrides reads `{"Standard_D4s_v6": {"hourly": 0.25, "spotHourly": 0.07}}`. Empty is none. A typo, a price that is not above zero, or a setting that
// is not known is an error, so a mistake cannot quietly leave a node at the list price.
func ParseOverrides(raw string) (map[string]Override, error) {
	out := map[string]Override{}
	if len(bytes.TrimSpace([]byte(raw))) == 0 {
		return out, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("cost prices: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("cost prices: something after the JSON object")
	}
	for name, o := range out {
		switch {
		case strings.TrimSpace(name) == "":
			return nil, errors.New("cost prices: an instance type has no name")
		case !(o.Hourly > 0) || math.IsInf(o.Hourly, 0):
			return nil, fmt.Errorf("cost prices: %s needs an hourly price above zero", name)
		case o.SpotHourly < 0 || math.IsNaN(o.SpotHourly) || math.IsInf(o.SpotHourly, 0):
			return nil, fmt.Errorf("cost prices: %s has a spot price that is not usable", name)
		}
	}
	return out, nil
}

const (
	azureURL        = "https://prices.azure.com/api/retail/prices"
	priceTTL        = 24 * time.Hour
	noPriceTTL      = time.Hour
	failureTTL      = 10 * time.Minute
	maxPriceBody    = 4 << 20
	maxPriceReplies = 3
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

type azItem struct {
	RetailPrice   float64 `json:"retailPrice"`
	ArmSkuName    string  `json:"armSkuName"`
	SkuName       string  `json:"skuName"`
	ProductName   string  `json:"productName"`
	UnitOfMeasure string  `json:"unitOfMeasure"`
	Type          string  `json:"type"`
}

type azPage struct {
	Items        []azItem `json:"Items"`
	NextPageLink string   `json:"NextPageLink"`
}

type cached struct {
	price float64
	err   error
	until time.Time
}

// AzureRetail reads list prices from the Azure Retail Prices API, and keeps what it learns: a day for a price, an hour for "there is none", ten minutes
// for a failure, so a page that is opened often does not ask every time and a lookup that failed is not repeated at once.
type AzureRetail struct {
	BaseURL string // azureURL when empty
	Client  *http.Client
	Timeout time.Duration // for one request; 10 seconds when zero
	Now     func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

func (a *AzureRetail) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Hourly is the Linux on-demand price per hour of the instance type in the region (an armRegionName such as "southindia"), in US dollars.
func (a *AzureRetail) Hourly(ctx context.Context, region, sku string) (float64, error) {
	if !nameRE.MatchString(region) || !nameRE.MatchString(sku) {
		return 0, fmt.Errorf("%w: %q in %q is not a name the price list takes", ErrNoPrice, sku, region)
	}
	key := region + "|" + sku
	a.mu.Lock()
	if c, ok := a.cache[key]; ok && a.now().Before(c.until) {
		a.mu.Unlock()
		return c.price, c.err
	}
	a.mu.Unlock()

	price, err := a.fetch(ctx, region, sku)
	ttl := priceTTL
	switch {
	case errors.Is(err, ErrNoPrice):
		ttl = noPriceTTL
	case err != nil:
		ttl = failureTTL
	}
	a.mu.Lock()
	if a.cache == nil {
		a.cache = map[string]cached{}
	}
	a.cache[key] = cached{price: price, err: err, until: a.now().Add(ttl)}
	a.mu.Unlock()
	return price, err
}

func (a *AzureRetail) fetch(ctx context.Context, region, sku string) (float64, error) {
	base := a.BaseURL
	if base == "" {
		base = azureURL
	}
	b, err := url.Parse(base)
	if err != nil {
		return 0, err
	}
	filter := fmt.Sprintf("serviceName eq 'Virtual Machines' and armRegionName eq '%s' and armSkuName eq '%s' and priceType eq 'Consumption'", region, sku)
	next := base + "?currencyCode=USD&$filter=" + url.QueryEscape(filter)
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	client := a.Client
	if client == nil {
		client = &http.Client{}
	}
	best := 0.0
	for page := 0; next != "" && page < maxPriceReplies; page++ {
		if u, err := url.Parse(next); err != nil || u.Scheme != b.Scheme || u.Host != b.Host {
			return 0, errors.New("the price list sent a link to somewhere else")
		}
		rctx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, next, nil)
		if err != nil {
			cancel()
			return 0, err
		}
		req.Header.Set("Accept", "application/json")
		res, err := client.Do(req)
		if err != nil {
			cancel()
			return 0, fmt.Errorf("the price list could not be reached: %w", errors.Unwrap(err))
		}
		body, rerr := io.ReadAll(io.LimitReader(res.Body, maxPriceBody+1))
		res.Body.Close()
		cancel()
		if rerr != nil {
			return 0, fmt.Errorf("the price list could not be read")
		}
		if res.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("the price list answered HTTP %d", res.StatusCode)
		}
		if len(body) > maxPriceBody {
			return 0, errors.New("the price list answered with more than it should")
		}
		var p azPage
		if err := json.Unmarshal(body, &p); err != nil {
			return 0, errors.New("the price list did not answer with prices")
		}
		for _, it := range p.Items {
			if !onDemandLinuxHour(it, sku) {
				continue
			}
			if best == 0 || it.RetailPrice < best {
				best = it.RetailPrice
			}
		}
		next = p.NextPageLink
	}
	if best == 0 {
		return 0, fmt.Errorf("%w: %s in %s", ErrNoPrice, sku, region)
	}
	return best, nil
}

// onDemandLinuxHour says an item is the price per hour of the type, Linux and not spot or low priority.
func onDemandLinuxHour(it azItem, sku string) bool {
	sk, prod := strings.ToLower(it.SkuName), strings.ToLower(it.ProductName)
	return it.RetailPrice > 0 && strings.EqualFold(it.ArmSkuName, sku) && strings.EqualFold(it.Type, "Consumption") && it.UnitOfMeasure == "1 Hour" &&
		!strings.Contains(sk, "spot") && !strings.Contains(sk, "low priority") && !strings.Contains(prod, "windows")
}

// Prices is what a node's price is looked up in.
type Prices struct {
	Overrides map[string]Override
	Azure     *AzureRetail // nil: no list prices
}

// Hourly is the price per hour of the instance type, where it came from ("override" or "azure-retail"), and an error when a lookup failed. No price at all
// is a nil price and no error: another cloud, or the list switched off.
func (p *Prices) Hourly(ctx context.Context, provider, region, instanceType, lifecycle string) (*float64, string, error) {
	for name, o := range p.Overrides {
		if strings.EqualFold(name, instanceType) {
			v := o.Hourly
			if strings.EqualFold(lifecycle, "spot") && o.SpotHourly > 0 {
				v = o.SpotHourly
			}
			return &v, "override", nil
		}
	}
	if p.Azure != nil && strings.EqualFold(provider, "azure") && instanceType != "" {
		v, err := p.Azure.Hourly(ctx, strings.ToLower(region), instanceType)
		if err != nil {
			return nil, "", err
		}
		return &v, "azure-retail", nil
	}
	return nil, "", nil
}
