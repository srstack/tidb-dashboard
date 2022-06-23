// Copyright 2022 PingCAP, Inc. Licensed under Apache-2.0.

package utils

import (
	"encoding/base64"
	"encoding/json"

	"github.com/golang/snappy"

	"github.com/pingcap/tipb/go-tipb"
)

// GenerateBinaryPlan generate visual plan from raw data.
func GenerateBinaryPlan(v string) (*tipb.ExplainData, error) {
	if v == "" {
		return nil, nil
	}

	// base64 decode
	compressVPBytes, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, err
	}

	// snappy uncompress
	vpBytes, err := snappy.Decode(nil, compressVPBytes)
	if err != nil {
		return nil, err
	}

	// proto unmarshal
	visual := &tipb.ExplainData{}
	err = visual.Unmarshal(vpBytes)
	if err != nil {
		return nil, err
	}
	return visual, nil
}

func GenerateBinaryPlanJSON(v string) (string, error) {
	// generate vp
	vp, err := GenerateBinaryPlan(v)
	if err != nil {
		return "", err
	}

	if vp == nil {
		return "", nil
	}

	// json marshal
	vpJSON, err := json.Marshal(vp)
	if err != nil {
		return "", err
	}

	return string(vpJSON), nil
}
