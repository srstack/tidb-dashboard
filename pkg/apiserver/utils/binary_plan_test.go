// Copyright 2022 PingCAP, Inc. Licensed under Apache-2.0.

package utils

import "testing"

var vpTestStr = "SiwKRgoGU2hvd18yKQAFAYjwPzAFOAFAAWoVdGltZTozNC44wrVzLCBsb29wczoygAH//w0COAGIAf///////////wEYAQ=="

func TestGenerateBinaryPlan(t *testing.T) {
	_, err := GenerateBinaryPlan(vpTestStr)
	if err != nil {
		t.Fatalf("generate binary plan failed: %v", err)
	}
}

func TestGenerateBinaryPlanJson(t *testing.T) {
	_, err := GenerateBinaryPlanJSON(vpTestStr)
	if err != nil {
		t.Fatalf("generate binary plan failed: %v", err)
	}
}
