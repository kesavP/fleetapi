package kube

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/sojoner/kro-lab/platform-mvp/cluster-collector/wire"
)

// The service refuses a whole batch for one object it does not like, so a single strange object would stop every report from a cluster for
// ever. Fit is the same checks, made here: an object that does not pass is left out and counted, and the rest are reported.

const maxData = 16 << 10 // bytes of an object's attributes, as JSON

var (
	uidRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,252}$`)
)

func allowed(kind string) map[string]bool {
	m := map[string]bool{}
	for _, a := range wire.Attributes[kind] {
		m[a] = true
	}
	return m
}

// Fit returns the object as it can be sent, or says why it cannot. It never changes what an attribute means: the only thing it does is cut
// the list of containers of a very large object down until the object fits.
func Fit(o wire.Object) (wire.Object, error) {
	ok := false
	for _, k := range wire.Kinds {
		ok = ok || k == o.Kind
	}
	switch {
	case !ok:
		return o, fmt.Errorf("kind %q is not one the service takes", o.Kind)
	case !uidRE.MatchString(o.UID):
		return o, fmt.Errorf("uid %q is not usable", o.UID)
	case !nameRE.MatchString(o.Name):
		return o, fmt.Errorf("name %q is not usable", o.Name)
	case wire.ClusterScoped[o.Kind] && o.Namespace != "":
		return o, fmt.Errorf("a %s has no namespace", o.Kind)
	case !wire.ClusterScoped[o.Kind] && !nameRE.MatchString(o.Namespace):
		return o, fmt.Errorf("namespace %q is not usable", o.Namespace)
	}
	allow := allowed(o.Kind)
	var extra []string
	for k := range o.Data {
		if !allow[k] {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		return o, fmt.Errorf("%s carries %s, which the service does not take", o.Kind, strings.Join(extra, ", "))
	}
	for keep := wire.MaxList; ; keep /= 2 {
		raw, err := json.Marshal(o.Data)
		if err != nil {
			return o, fmt.Errorf("attributes cannot be encoded: %v", err)
		}
		if len(raw) <= maxData {
			return o, nil
		}
		cs, isList := o.Data["containers"].([]any)
		if !isList || len(cs) <= 1 || keep < 1 {
			return o, fmt.Errorf("attributes are %d bytes; at most %d", len(raw), maxData)
		}
		if keep >= len(cs) {
			keep = len(cs) / 2
		}
		d := make(map[string]any, len(o.Data))
		for k, v := range o.Data {
			d[k] = v
		}
		d["containers"] = cs[:keep]
		o.Data = d
	}
}
