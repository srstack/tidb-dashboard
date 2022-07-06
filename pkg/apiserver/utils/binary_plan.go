// Copyright 2022 PingCAP, Inc. Licensed under Apache-2.0.

package utils

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	simplejson "github.com/bitly/go-simplejson"
	"github.com/golang/snappy"
	"github.com/pingcap/tipb/go-tipb"
	json "google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/runtime/protoimpl"
)

const (
	MainTree          = "main"
	CteTrees          = "ctes"
	Children          = "children"
	Duration          = "duration"
	Time              = "time"
	Diagnosis         = "diagnosis"
	RootGroupExecInfo = "rootGroupExecInfo"
	RootBasicExecInfo = "rootBasicExecInfo"
	OperatorInfo      = "operatorInfo"
	OperatorName      = "name"
	CopExecInfo       = "copExecInfo"
	CacheHitRatio     = "cacheHitRatio"
	TaskType          = "taskType"
	StoreType         = "storeType"
	DiskBytes         = "diskBytes"
	MemoryBytes       = "memoryBytes"
	ActRows           = "actRows"
	EstRows           = "estRows"
	DriverSide        = "driverSide"
	ScanObject        = "scanObject"

	JoinTaskThreshold    = 10000
	returnTableThreshold = 0.7
)

// operator.
type operator int

const (
	Default operator = iota
	IndexJoin
	IndexMergeJoin
	IndexHashJoin
	Apply
	Shuffle
	ShuffleReceiver
	IndexLookUpReader
	IndexMergeReader
	IndexFullScan
	IndexRangeScan
	TableFullScan
	TableRangeScan
	TableRowIDScan
	Selection
)

type concurrency struct {
	joinConcurrency    int
	copConcurrency     int
	tableConcurrency   int
	applyConcurrency   int
	shuffleConcurrency int
}

type diagnosticOperation struct {
	needUdateStatistics bool
}

var (
	needJSONFormat = []string{
		RootGroupExecInfo,
		RootBasicExecInfo,
		CopExecInfo,
	}

	needSetNA = []string{
		MemoryBytes,
		DiskBytes,
	}

	needCheckOperator = []string{
		"eq", "ge", "gt", "le", "lt", "isnull", "in",
	}
)

func newConcurrency() concurrency {
	return concurrency{
		joinConcurrency:    1,
		copConcurrency:     1,
		tableConcurrency:   1,
		applyConcurrency:   1,
		shuffleConcurrency: 1,
	}
}

func newDiagnosticOperation() diagnosticOperation {
	return diagnosticOperation{}
}

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
	bpBytes, err := snappy.Decode(nil, compressVPBytes)
	if err != nil {
		return nil, err
	}

	// proto unmarshal
	bp := &tipb.ExplainData{}
	err = bp.Unmarshal(bpBytes)
	if err != nil {
		return nil, err
	}
	return bp, nil
}

func GenerateBinaryPlanJSON(b string) (string, error) {
	// generate bp
	bp, err := GenerateBinaryPlan(b)
	if err != nil {
		return "", err
	}

	if bp == nil {
		return "", nil
	}

	// json marshal
	bpJSON, err := json.Marshal(protoimpl.X.ProtoMessageV2Of(bp))
	if err != nil {
		return "", err
	}

	bpJSON, err = formatBinaryPlanJSON(bpJSON)
	if err != nil {
		return "", err
	}

	bpJSON, err = analyzeDuration(bpJSON)
	if err != nil {
		return "", err
	}

	bpJSON, err = diagnosticOperator(bpJSON)
	if err != nil {
		return "", err
	}

	return string(bpJSON), nil
}

func diagnosticOperator(bp []byte) ([]byte, error) {
	// new simple json
	vp, err := simplejson.NewJson(bp)
	if err != nil {
		return nil, err
	}

	// main
	_, err = diagnosticOperatorNode(vp.Get(MainTree), newDiagnosticOperation())
	if err != nil {
		return nil, err
	}

	// ctes
	_, err = diagnosticOperatorNodes((vp.Get(CteTrees)), newDiagnosticOperation())
	if err != nil {
		return nil, err
	}

	return vp.MarshalJSON()
}

// diagnosticOperatorNode set node.diagnosis.
func diagnosticOperatorNode(node *simplejson.Json, diagOp diagnosticOperation) (diagnosticOperation, error) {
	operator := getOperatorType(node)
	operatorInfo := node.Get(OperatorInfo).MustString()
	diagnosis := []string{}

	// pseudo stats
	if strings.Contains(operatorInfo, "stats:pseudo") {
		switch strings.ToLower(node.GetPath(ScanObject, "database").MustString()) {
		case "information_schema", "metrics_schema", "performance_schema", "mysql":
			break
		default:
			diagnosis = append(diagnosis, "This operator used pseudo statistics and the estimation might be inaccurate. It might be caused by unavailable or outdated statistics. Consider collecting statistics or setting variable tidb_enable_pseudo_for_outdated_stats to OFF.")
		}
	}

	// use disk
	diskBytes := node.Get(DiskBytes).MustString()
	if diskBytes != "N/A" {
		diagnosis = append(diagnosis, "Disk spill is triggered for this operator because the memory quota is exceeded. The execution might be slow. Consider increasing the memory quota if there's enough memory.")
	}

	diagOp, err := diagnosticOperatorNodes(node.Get(Children), diagOp)

	// marked rows estimation error too high
	if diagOp.needUdateStatistics {
		switch operator {
		case IndexFullScan, IndexRangeScan, TableFullScan, TableRangeScan, TableRowIDScan, Selection:
			actRows := node.Get(ActRows).MustFloat64()
			estRows := node.Get(EstRows).MustFloat64()
			if actRows == 0 || estRows == 0 {
				actRows = +1
				estRows = +1
			}
			if actRows/estRows > 100 || estRows/actRows > 100 {
				diagnosis = append(diagnosis, "The estimation error is high. Consider checking the health state of the statistics.")
			}
			diagOp.needUdateStatistics = true
		default:
			diagOp.needUdateStatistics = false
		}
	}

	switch operator {
	// index join
	case IndexJoin, IndexMergeJoin, IndexHashJoin:
		// only use in build
		rootGroupInfo := node.Get(RootGroupExecInfo)
		rootGroupInfoCount := len(rootGroupInfo.MustArray())
		if rootGroupInfoCount > 0 {
			for i := 0; i < rootGroupInfoCount; i++ {
				joinTaskCountStr := rootGroupInfo.GetIndex(i).GetPath("inner", "task").MustString()
				joinTaskCount, _ := strconv.Atoi(joinTaskCountStr)
				if joinTaskCount > JoinTaskThreshold {
					diagnosis = append(diagnosis, "This index join has a large build side. It might be slow and cause heavy pressure on TiKV. Consider using the optimizer hints to guide the optimizer to choose hash join.")
					break
				}
			}
		}
	// unreasonable index return table plan
	case IndexLookUpReader:
		cNode := getBuildChildrenWithDriverSide(node, "build")
		if getOperatorType(cNode) != Selection {
			break
		}
		if len(cNode.Get(Children).MustArray()) != 1 {
			break
		}
		gNode := cNode.Get(Children).GetIndex(0)

		if getOperatorType(gNode) != IndexFullScan {
			break
		}

		if !strings.Contains(gNode.Get(OperatorInfo).MustString(), "keep order:false") {
			break
		}

		cNodeActRows := cNode.Get(ActRows).MustFloat64()
		gNodeActRows := gNode.Get(ActRows).MustFloat64()
		if cNodeActRows/gNodeActRows > returnTableThreshold {
			diagnosis = append(diagnosis, "This IndexLookup read a lot of data from the index side. It might be slow and cause heavy pressure on TiKV. Consider using the optimizer hints to guide the optimizer to choose a better index or not to use index.")
		}
	case Selection:
		if len(node.Get(Children).MustArray()) != 1 {
			break
		}
		cNode := node.Get(Children).GetIndex(0)

		if getOperatorType(cNode) != IndexFullScan {
			break
		}
		if node.Get(StoreType).MustString() != "tikv" {
			break
		}

		if !useComparisonOperator(operatorInfo) {
			break
		}

		if node.Get(ActRows).MustFloat64() < 10000 && cNode.Get(ActRows).MustFloat64() > 5000000 {
			diagnosis = append(diagnosis, "This Selection filters a high proportion of data. Using an index on this column might achieve better performance. Consider adding an index on this column if there is not one.")
		}

	case TableFullScan:
		if node.Get(StoreType).MustString() == "tikv" && node.Get(ActRows).MustFloat64() > 1000000000 {
			diagnosis = append(diagnosis, "The TiKV read a lot of data. Consider using TiFlash to get better performance if it's necessary to read so much data.")
		}
	}

	// set diagnosis
	node.Set(Diagnosis, diagnosis)
	if err != nil {
		return diagOp, err
	}

	return diagOp, nil
}

func diagnosticOperatorNodes(nodes *simplejson.Json, diagOp diagnosticOperation) (diagnosticOperation, error) {
	length := len(nodes.MustArray())

	// no children nodes
	if length == 0 {
		diagOp.needUdateStatistics = true
		return diagOp, nil
	}
	var needUdateStatistics bool
	for i := 0; i < length; i++ {
		c := nodes.GetIndex(i)
		n, err := diagnosticOperatorNode(c, diagOp)
		if err != nil {
			return diagOp, err
		}
		if n.needUdateStatistics {
			needUdateStatistics = n.needUdateStatistics
		}
	}

	diagOp.needUdateStatistics = needUdateStatistics
	return diagOp, nil
}

// cut go 1.18 strings.Cut.
func cut(s, sep string) (before, after string, found bool) {
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}

// useComparisonOperator
// matching rules: only match the eq/ge/gt/le/lt/isnull/in functions on a single column
// for example:
// eq(test.t.a, 1)  ture
// eq(minus(test.t1.b, 1), 1) false
// eq(test.t.a, 1), eq(test.t.a, 2)  ture
// eq(test.t.a, 1), eq(test.t.b, 1) false
// in(test.t.a, 1, 2, 3, 4) ture
// in(test.t.a, 1, 2, 3, 4), in(test.t.b, 1, 2, 3, 4) false.
func useComparisonOperator(operatorInfo string) bool {
	useComparisonOperator := false
	columnSet := make(map[string]bool)
	for _, op := range needCheckOperator {
		if strings.Contains(operatorInfo, op) {
			useComparisonOperator = true
			n := strings.Count(operatorInfo, op+"(")
			for i := 0; i < n; i++ {
				column := ""
				s1, s, _ := cut(operatorInfo, op+"(")
				s, s2, _ := cut(s, ")")

				if strings.Contains(s, "(") {
					return false
				}
				switch op {
				case "isnull":
					// not(isnull(test2.t1.a)) true
					if strings.HasSuffix(s1, "not(") && strings.HasPrefix(s2, ")") {
						s1 = strings.TrimRight(s1, "not(")
						s2 = strings.TrimLeft(s2, ")")
					}
					// isnull(test.t.a)
					column = strings.TrimSpace(s)
					// if strings.
				case "in":
					// in(test.t.a, 1, 2, 3, 4) true
					slist := strings.Split(s, ",")
					column = strings.TrimSpace(slist[0])
					// in(test.t.a, 1, 2, test.t.b, 4) false
					for _, c := range slist[1:] {
						c = strings.TrimSpace(c)
						if strings.Count(c, ".") == 2 && !strings.Contains(c, `"`) {
							return false
						}
					}
				default:
					// eq(test.t.a, 1) true
					slist := strings.Split(s, ",")
					if len(slist) != 2 {
						return false
					}
					c := ""
					c1, c2 := strings.TrimSpace(slist[0]), strings.TrimSpace(slist[1])
					if strings.Count(c1, ".") == 2 && !strings.Contains(c1, `"`) {
						column = c1
						c = c2
					} else if strings.Count(c2, ".") == 2 && !strings.Contains(c2, `"`) {
						column = c2
						c = c1
					}
					// eq(test2.t1.a, test2.t2.a) false
					database := strings.Split(column, ".")[0]
					if len(slist) > 1 && strings.HasPrefix(c, database) {
						return false
					}
				}

				if column != "" {
					columnSet[column] = true
				}
				operatorInfo = s1 + s2
			}
		}
	}
	if useComparisonOperator {
		if strings.Count(operatorInfo, "(") == strings.Count(operatorInfo, ")") && strings.Count(operatorInfo, "(") > 0 {
			return false
		}
		// single column
		if len(columnSet) != 1 {
			return false
		}
	}

	return useComparisonOperator
}

func analyzeDuration(bp []byte) ([]byte, error) {
	// new simple json
	vp, err := simplejson.NewJson(bp)
	if err != nil {
		return nil, err
	}

	rootTs := vp.Get(MainTree).GetPath(RootBasicExecInfo, Time).MustString()
	// main
	mainConcurrency := newConcurrency()
	_, err = analyzeDurationNode(vp.Get(MainTree), mainConcurrency)
	if err != nil {
		return nil, err
	}
	vp.Get(MainTree).Set(Duration, rootTs)

	// ctes
	ctesConcurrency := newConcurrency()
	_, err = analyzeDurationNodes(vp.Get(CteTrees), Default, ctesConcurrency)
	if err != nil {
		return nil, err
	}

	return vp.MarshalJSON()
}

// analyzeDurationNode set node.duration.
func analyzeDurationNode(node *simplejson.Json, concurrency concurrency) (time.Duration, error) {
	// get duration time
	ts := node.GetPath(RootBasicExecInfo, Time).MustString()

	// cop task
	if ts == "" {
		ts = getCopTaskDuratuon(node, concurrency)
	} else {
		ts = getOperatorDuratuon(ts, concurrency)
	}

	operator := getOperatorType(node)
	duration, err := time.ParseDuration(ts)
	if err != nil {
		duration = 0
	}
	// get current_node concurrency
	concurrency = getConcurrency(node, operator, concurrency)

	subDuration, err := analyzeDurationNodes(node.Get(Children), operator, concurrency)
	if err != nil {
		return 0, err
	}

	if duration < subDuration {
		duration = subDuration
	}

	// set
	node.Set(Duration, duration.String())

	return duration, nil
}

// analyzeDurationNodes return max(node.duration).
func analyzeDurationNodes(nodes *simplejson.Json, operator operator, concurrency concurrency) (time.Duration, error) {
	length := len(nodes.MustArray())

	// no children nodes
	if length == 0 {
		return 0, nil
	}
	var durations []time.Duration

	if operator == Apply {
		for i := 0; i < length; i++ {
			n := nodes.GetIndex(i)
			if n.Get(DriverSide).MustString() == "build" {
				newConcurrency := concurrency
				newConcurrency.applyConcurrency = 1
				d, err := analyzeDurationNode(n, newConcurrency)
				if err != nil {
					return 0, err
				}
				durations = append(durations, d)

				// get probe concurrency
				var cacheHitRatio, actRows float64
				rootGroupInfo := n.Get(RootGroupExecInfo)
				for i := 0; i < len(rootGroupInfo.MustArray()); i++ {
					cacheHitRatioStr := strings.TrimRight(rootGroupInfo.GetIndex(i).Get(CacheHitRatio).MustString(), "%")
					if cacheHitRatioStr == "" {
						continue
					}
					cacheHitRatio, err = strconv.ParseFloat(cacheHitRatioStr, 64)
					if err != nil {
						return 0, err
					}
				}

				actRows, err = strconv.ParseFloat(n.Get(ActRows).MustString(), 64)
				if err != nil {
					return 0, err
				}

				taskCount := int(actRows * (1 - cacheHitRatio/100))

				if taskCount < concurrency.applyConcurrency {
					concurrency.applyConcurrency = taskCount
				}

				break
			}
		}

		for i := 0; i < length; i++ {
			n := nodes.GetIndex(i)
			if n.Get(DriverSide).MustString() == "probe" {
				d, err := analyzeDurationNode(n, concurrency)
				if err != nil {
					return 0, err
				}
				durations = append(durations, d)
				break
			}
		}
	} else {
		for i := 0; i < length; i++ {
			var d time.Duration
			var err error
			n := nodes.GetIndex(i)

			switch operator {
			case IndexJoin, IndexMergeJoin, IndexHashJoin:
				if n.Get(DriverSide).MustString() == "probe" {
					d, err = analyzeDurationNode(n, concurrency)
				} else {
					// build: set joinConcurrency == 1
					newConcurrency := concurrency
					newConcurrency.joinConcurrency = 1
					d, err = analyzeDurationNode(n, newConcurrency)
				}
			case IndexLookUpReader, IndexMergeReader:
				if n.Get(DriverSide).MustString() == "probe" {
					d, err = analyzeDurationNode(n, concurrency)
				} else {
					// build: set joinConcurrency == 1
					newConcurrency := concurrency
					newConcurrency.tableConcurrency = 1
					d, err = analyzeDurationNode(n, newConcurrency)
				}
			// concurrency:  suffle -> StreamAgg/Window/MergeJoin ->  Sort -> ShuffleReceiver
			case ShuffleReceiver:
				newConcurrency := concurrency
				newConcurrency.shuffleConcurrency = 1
				d, err = analyzeDurationNode(n, newConcurrency)
			default:
				d, err = analyzeDurationNode(n, concurrency)
			}

			if err != nil {
				return 0, err
			}
			durations = append(durations, d)
		}
	}

	// get max duration
	sort.Slice(durations, func(p, q int) bool {
		return durations[p] > durations[q]
	})

	return durations[0], nil
}

func getOperatorType(node *simplejson.Json) operator {
	operator := node.Get(OperatorName).MustString()

	switch {
	case strings.HasPrefix(operator, "IndexJoin"):
		return IndexJoin
	case strings.HasPrefix(operator, "IndexMergeJoin"):
		return IndexMergeJoin
	case strings.HasPrefix(operator, "IndexHashJoin"):
		return IndexHashJoin
	case strings.HasPrefix(operator, "Apply"):
		return Apply
	case strings.HasPrefix(operator, "Shuffle") && !strings.Contains(operator, "ShuffleReceiver"):
		return Shuffle
	case strings.HasPrefix(operator, "ShuffleReceiver"):
		return ShuffleReceiver
	case strings.HasPrefix(operator, "IndexLookUp"):
		return IndexLookUpReader
	case strings.HasPrefix(operator, "IndexMerge"):
		return IndexMergeReader
	case strings.HasPrefix(operator, "IndexFullScan"):
		return IndexFullScan
	case strings.HasPrefix(operator, "IndexRangeScan"):
		return IndexRangeScan
	case strings.HasPrefix(operator, "TableFullScan"):
		return TableFullScan
	case strings.HasPrefix(operator, "TableRangeScan"):
		return TableRangeScan
	case strings.HasPrefix(operator, "TableRowIDScan"):
		return TableRowIDScan
	case strings.HasPrefix(operator, "Selection"):
		return Selection
	default:
		return Default
	}
}

func getBuildChildrenWithDriverSide(node *simplejson.Json, driverSide string) *simplejson.Json {
	nodes := node.Get(Children)
	length := len(nodes.MustArray())

	// no children nodes
	if length == 0 {
		return nil
	}

	for i := 0; i < length; i++ {
		n := nodes.GetIndex(i)
		if n.Get(DriverSide).MustString() == driverSide {
			return n
		}
	}
	return nil
}

func getConcurrency(node *simplejson.Json, operator operator, concurrency concurrency) concurrency {
	// concurrency, copConcurrency
	rootGroupInfo := node.Get(RootGroupExecInfo)
	rootGroupInfoCount := len(rootGroupInfo.MustArray())
	if rootGroupInfoCount > 0 {
		for i := 0; i < rootGroupInfoCount; i++ {
			switch operator {
			case IndexJoin, IndexMergeJoin, IndexHashJoin:
				tmpJoinConcurrencyStr := rootGroupInfo.GetIndex(i).GetPath("inner", "concurrency").MustString()
				tmpJoinConcurrency, _ := strconv.Atoi(tmpJoinConcurrencyStr)

				joinTaskCountStr := rootGroupInfo.GetIndex(i).GetPath("inner", "task").MustString()
				joinTaskCount, _ := strconv.Atoi(joinTaskCountStr)

				// task count as concurrency
				if joinTaskCount < tmpJoinConcurrency {
					tmpJoinConcurrency = joinTaskCount
				}

				if tmpJoinConcurrency > 0 {
					concurrency.joinConcurrency = tmpJoinConcurrency * concurrency.joinConcurrency
				}

			case Apply:
				tmpApplyConcurrencyStr := rootGroupInfo.GetIndex(i).GetPath("Concurrency").MustString()
				tmpApplyConcurrency, _ := strconv.Atoi(tmpApplyConcurrencyStr)
				if tmpApplyConcurrency > 0 {
					concurrency.applyConcurrency = tmpApplyConcurrency * concurrency.applyConcurrency
				}

			case IndexLookUpReader, IndexMergeReader:
				tmpTableConcurrencyStr := rootGroupInfo.GetIndex(i).GetPath("table_task", "concurrency").MustString()
				tmpTableConcurrency, _ := strconv.Atoi(tmpTableConcurrencyStr)

				tableTaskNumStr := rootGroupInfo.GetIndex(i).GetPath("table_task", "num").MustString()
				tableTaskNum, _ := strconv.Atoi(tableTaskNumStr)
				tableTaskNum = tableTaskNum / concurrency.joinConcurrency
				if tableTaskNum < tmpTableConcurrency {
					tmpTableConcurrency = tableTaskNum
				}

				if tmpTableConcurrency > 0 {
					concurrency.tableConcurrency = tmpTableConcurrency * concurrency.copConcurrency
				}

			case Shuffle:
				tmpSuffleConcurrencyStr := rootGroupInfo.GetIndex(i).Get("ShuffleConcurrency").MustString()
				tmpSuffleConcurrency, _ := strconv.Atoi(tmpSuffleConcurrencyStr)

				if tmpSuffleConcurrency > 0 {
					concurrency.shuffleConcurrency = tmpSuffleConcurrency * concurrency.shuffleConcurrency
				}
			}

			tmpCopConcurrencyStr := rootGroupInfo.GetIndex(i).GetPath("cop_task", "distsql_concurrency").MustString()
			tmpCopConcurrency, _ := strconv.Atoi(tmpCopConcurrencyStr)
			if tmpCopConcurrency > 0 {
				concurrency.copConcurrency = tmpCopConcurrency * concurrency.copConcurrency
			}
		}
	}

	return concurrency
}

func getCopTaskDuratuon(node *simplejson.Json, concurrency concurrency) string {
	storeType := node.GetPath(StoreType).MustString()
	// task == 1
	ts := node.GetPath(CopExecInfo, fmt.Sprintf("%s_task", storeType), "time").MustString()
	if ts == "" {
		switch node.GetPath(TaskType).MustString() {
		case "cop":
			// cop task count
			taskCountStr := node.GetPath(CopExecInfo, fmt.Sprintf("%s_task", storeType), "tasks").MustString()
			taskCount, _ := strconv.Atoi(taskCountStr)
			maxTS := node.GetPath(CopExecInfo, fmt.Sprintf("%s_task", storeType), "proc max").MustString()
			maxDuration, err := time.ParseDuration(maxTS)
			if err != nil {
				ts = maxTS
				break
			}
			avgTS := node.GetPath(CopExecInfo, fmt.Sprintf("%s_task", storeType), "avg").MustString()
			avgDuration, err := time.ParseDuration(avgTS)
			if err != nil {
				ts = maxTS
				break
			}

			var tsDuration time.Duration
			n := float64(taskCount) / float64(
				concurrency.joinConcurrency*concurrency.tableConcurrency*concurrency.applyConcurrency*concurrency.shuffleConcurrency*concurrency.copConcurrency)

			if n > 1 {
				tsDuration = time.Duration(float64(avgDuration) * n)
			} else {
				tsDuration = time.Duration(float64(avgDuration) /
					float64(concurrency.joinConcurrency*concurrency.tableConcurrency*concurrency.applyConcurrency*concurrency.shuffleConcurrency))
			}

			ts = tsDuration.String()

			if tsDuration > maxDuration {
				ts = maxTS
			}
		// tiflash
		case "batchCop", "mpp":
			ts = node.GetPath(CopExecInfo, fmt.Sprintf("%s_task", storeType), "proc max").MustString()
		default:
			ts = "0s"
		}
	}

	return ts
}

func getOperatorDuratuon(ts string, concurrency concurrency) string {
	t, err := time.ParseDuration(ts)
	if err != nil {
		return "0s"
	}

	return time.Duration(float64(t) /
		float64(concurrency.joinConcurrency*concurrency.tableConcurrency*concurrency.applyConcurrency*concurrency.shuffleConcurrency)).
		String()
}

func formatBinaryPlanJSON(bp []byte) ([]byte, error) {
	// new simple json
	vp, err := simplejson.NewJson(bp)
	if err != nil {
		return nil, err
	}

	// main
	err = formatNode(vp.Get(MainTree))
	if err != nil {
		return nil, err
	}

	// ctes
	err = formatChildrenNodes(vp.Get(CteTrees))
	if err != nil {
		return nil, err
	}

	return vp.MarshalJSON()
}

// formatNode
// format diskBytes memoryByte to string
// format rootBasicExecInfo rootGroupExecInfo copExecInfo field to json
// for example:
// {"copExecInfo" : "tikv_task:{time:0s, loops:1}, scan_detail: {total_process_keys: 8, total_process_keys_size: 360, total_keys: 9, rocksdb: {delete_skipped_count: 0, key_skipped_count: 8, block: {cache_hit_count: 1, read_count: 0, read_byte: 0 Bytes}}}"}.
func formatNode(node *simplejson.Json) error {
	for _, key := range needSetNA {
		if node.Get(key).MustString() == "-1" {
			node.Set(key, "N/A")
		}
	}
	var err error

	for _, key := range needJSONFormat {
		if key == RootGroupExecInfo {
			slist := node.Get(key).MustStringArray()
			newSlist := []interface{}{}
			for _, s := range slist {
				sJSON, err := formatJSON(s)
				if err != nil {
					newSlist = append(newSlist, s)
				}
				newSlist = append(newSlist, sJSON)
			}
			node.Set(key, newSlist)
		} else {
			s := node.Get(key).MustString()
			sJSON, err := formatJSON(s)
			if err != nil {
				continue
			}
			node.Set(key, sJSON)
		}
	}

	c := node.Get(Children)
	err = formatChildrenNodes(c)
	if err != nil {
		return err
	}

	return nil
}

func formatChildrenNodes(nodes *simplejson.Json) error {
	length := len(nodes.MustArray())

	// no children nodes
	if length == 0 {
		return nil
	}

	for i := 0; i < length; i++ {
		c := nodes.GetIndex(i)
		err := formatNode(c)
		if err != nil {
			return err
		}
	}

	return nil
}

func formatJSON(s string) (*simplejson.Json, error) {
	s = `{` + s + `}`
	s = strings.ReplaceAll(s, "{", `{"`)
	s = strings.ReplaceAll(s, "}", `"}`)
	s = strings.ReplaceAll(s, ":", `":"`)
	s = strings.ReplaceAll(s, ",", `","`)
	s = strings.ReplaceAll(s, `" `, `"`)
	s = strings.ReplaceAll(s, `}"`, `}`)
	s = strings.ReplaceAll(s, `"{`, `{`)
	s = strings.ReplaceAll(s, `{""}`, "{}")

	return simplejson.NewJson([]byte(s))
}
