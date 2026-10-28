package controller

import (
	"testing"
)

func TestGVKConstants(t *testing.T) {
	if RegionalWidgetRequestGVK.Group != "platform.example.com" {
		t.Errorf("unexpected group: %s", RegionalWidgetRequestGVK.Group)
	}
	if RegionalWidgetRequestGVK.Kind != "RegionalWidgetRequest" {
		t.Errorf("unexpected kind: %s", RegionalWidgetRequestGVK.Kind)
	}
}
