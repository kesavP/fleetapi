package costs

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// The analysis says where the money goes that did not have to, in the order of what matters: the savings (what nobody requested, what is requested and not
// used, and how few nodes the requests would fit on), then what limits what can be said (no requests, no price, a lifecycle that is a guess). A finding is
// only made when it is material, so a cluster that is used well gets none.
const (
	materialIdle = 0.10 // idle is a finding from a tenth of what the nodes cost
	materialOver = 0.05 // over-provisioned requests from a twentieth
	topItems     = 5
)

func money(v float64) string {
	if v >= 100 {
		return fmt.Sprintf("$%.0f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

func pct(share float64) string { return fmt.Sprintf("%.0f%%", math.Round(share*100)) }

func names(items []FindingItem, n int) string {
	var out []string
	for i, it := range items {
		if i == n {
			break
		}
		out = append(out, fmt.Sprintf("%s (%s)", it.Name, money(it.Monthly)))
	}
	return strings.Join(out, ", ")
}

func analyse(r Report, in Inputs) []Finding {
	out := []Finding{}

	// idle: what nobody requested (or, with no requests, what nobody used)
	if r.Totals.PriceMonthly > 0 && r.Totals.IdleMonthly/r.Totals.PriceMonthly >= materialIdle {
		var items []FindingItem
		for _, n := range r.Nodes { // already the most idle first
			if n.PriceMonthly != nil && n.IdleMonthly > 0 && len(items) < topItems {
				items = append(items, FindingItem{Name: n.Name, Monthly: n.IdleMonthly})
			}
		}
		share := r.Totals.IdleMonthly / r.Totals.PriceMonthly
		f := Finding{ID: "idle-capacity", Kind: "saving", MonthlySaving: ptr(r.Totals.IdleMonthly), Items: items}
		if r.HasRequests {
			f.Title = fmt.Sprintf("Nobody requested %s of what the nodes cost: %s a month", pct(share), money(r.Totals.IdleMonthly))
			f.Detail = "This is capacity the nodes have and are paid for that no pod has been promised. The most is on " + names(items, 3) + ". Fewer or smaller nodes, or placing more workloads on them, would use it."
		} else {
			f.Title = fmt.Sprintf("%s of what the nodes cost was not used: %s a month", pct(share), money(r.Totals.IdleMonthly))
			f.Detail = "This is capacity that nothing used over the window. The most is on " + names(items, 3) + ". What pods request is not known, so what was set aside for them cannot be told from what is free."
		}
		out = append(out, f)
	}

	if r.HasRequests {
		// requests that are far above use
		if r.Totals.AppOverprovisionMonthly != nil && r.Totals.PriceMonthly > 0 && *r.Totals.AppOverprovisionMonthly/r.Totals.PriceMonthly >= materialOver {
			apps := append([]App(nil), r.Applications...)
			sort.SliceStable(apps, func(i, j int) bool {
				return deref(apps[i].OverprovisioningMonthly) > deref(apps[j].OverprovisioningMonthly)
			})
			var items []FindingItem
			for _, a := range apps {
				if v := deref(a.OverprovisioningMonthly); v > 0 && len(items) < topItems {
					items = append(items, FindingItem{Name: a.Label, Monthly: v})
				}
			}
			out = append(out, Finding{
				ID: "overprovisioned-requests", Kind: "saving", MonthlySaving: ptr(*r.Totals.AppOverprovisionMonthly), Items: items,
				Title:  fmt.Sprintf("Applications request %s a month more than they use", money(*r.Totals.AppOverprovisionMonthly)),
				Detail: "Requests reserve node capacity whether it is used or not. The most is in " + names(items, 3) + ". Lowering a request toward what is used, plus headroom, frees the capacity; the right-sizing rules (Configuration, Rules) show a limit to try for each container.",
			})
		}
		// how few nodes would do
		if f, ok := consolidation(r); ok {
			out = append(out, f)
		}
	} else {
		out = append(out, Finding{
			ID: "requests-unknown", Kind: "info", Items: []FindingItem{},
			Title:  "What pods request is not known",
			Detail: "The cluster collector is not reporting, so applications have no allocation or over-provisioning cost and idle is only what was not used. Deploy the cluster collector to see what is requested and not used.",
		})
	}

	// what limits what can be said
	var priceless []string
	for _, n := range r.Nodes {
		if n.PriceMonthly == nil {
			priceless = append(priceless, n.Name)
		}
	}
	if len(priceless) > 0 {
		out = append(out, Finding{
			ID: "price-missing", Kind: "warning", Items: []FindingItem{},
			Title:  fmt.Sprintf("%d node%s have no price", len(priceless), plural(len(priceless))),
			Detail: "No price was found for " + strings.Join(priceless, ", ") + ", so they are left out of every figure here. Set one for its instance type in the hub's cost prices.",
		})
	}
	for _, n := range r.Nodes {
		if n.PriceMonthly != nil && n.LifecycleAssumed {
			out = append(out, Finding{
				ID: "lifecycle-assumed", Kind: "info", Items: []FindingItem{},
				Title:  "Nodes are priced as on-demand",
				Detail: "The cloud does not say whether a node is spot or on-demand, so every node is priced as on-demand. Spot nodes cost much less: set a price for the instance type if some are.",
			})
			break
		}
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// consolidation asks how few nodes would carry what is requested (or used, where that is more), with a fifth of each kept free, and never fewer than two.
func consolidation(r Report) (Finding, bool) {
	var n int
	var price, capCPU, capMem, demandCPU, demandMem float64
	var priced int
	for _, nd := range r.Nodes {
		n++
		capCPU += nd.CPU.Capacity
		capMem += nd.Memory.Capacity
		demandCPU += math.Max(deref(nd.CPU.Requests), nd.CPU.Usage)
		demandMem += math.Max(deref(nd.Memory.Requests), nd.Memory.Usage)
		if nd.PriceMonthly != nil {
			price += *nd.PriceMonthly
			priced++
		}
	}
	if n < minNodes+1 || priced == 0 || capCPU <= 0 || capMem <= 0 {
		return Finding{}, false
	}
	avgCPU, avgMem, avgPrice := capCPU/float64(n), capMem/float64(n), price/float64(priced)
	usable := 1 - consolidationHeadroom
	need := max(minNodes, int(math.Ceil(demandCPU/(avgCPU*usable)-1e-9)), int(math.Ceil(demandMem/(avgMem*usable)-1e-9)))
	if need >= n {
		return Finding{}, false
	}
	saving := float64(n-need) * avgPrice
	return Finding{
		ID: "consolidation", Kind: "saving", MonthlySaving: ptr(saving), Items: []FindingItem{},
		Title: fmt.Sprintf("What is requested would fit on %d of the %d nodes: %s a month", need, n, money(saving)),
		Detail: fmt.Sprintf("Counting what is requested (or used, where that is more) and keeping %s of each node free, the workloads need about %d nodes' worth: removing %d could save %s a month. "+
			"Check pod anti-affinity, disruption budgets, node pools and spikes before scaling down.", pct(consolidationHeadroom), need, n-need, money(saving)),
	}, true
}
