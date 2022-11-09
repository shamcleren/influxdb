package httpd

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gogo/protobuf/proto"
	"github.com/gogo/protobuf/types"
	"github.com/golang/snappy"
	"github.com/influxdata/influxdb/prometheus"
	"github.com/influxdata/influxdb/prometheus/remote"
	"github.com/influxdata/influxdb/services/meta"
	"github.com/influxdata/influxdb/services/storage"
	"github.com/influxdata/influxdb/storage/reads"
	"github.com/influxdata/influxdb/storage/reads/datatypes"
	"github.com/influxdata/influxdb/tsdb"
	"github.com/influxdata/influxql"

	"net/http"
	"strings"
	"time"
)

const (
	measurementTagKey = "_measurement"
	fieldTagKey       = "_field"

	ContentTypeProtobuf = "application/x-protobuf"
	ContentTypeJson     = "application/json"

	ContentEncodingSnappy = "snappy"
)

func (h *Handler) serveRawRead(w http.ResponseWriter, r *http.Request, user meta.User) {
	rw, ok := w.(ResponseWriter)
	if !ok {
		rw = NewResponseWriter(w, r)
	}

	db := r.FormValue("db")
	rp := r.FormValue("rp")
	measurement := r.FormValue("measurement")
	field := r.FormValue("field")
	where := strings.TrimSpace(r.FormValue("where"))

	readRequest, err := GetReadRequest(db, rp, measurement, field, where)
	if err != nil {
		h.httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	ctx := context.Background()
	rs, err := h.Store.ReadFilter(ctx, readRequest)
	if err != nil {
		h.httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	readResponse, err := GetReadResponse(rs)
	if err != nil {
		h.httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	formatWriter := &FormatWriter{
		ctx: ctx, w: rw, r: r,
	}
	err = formatWriter.Response(readResponse)
	if err != nil {
		h.httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

type FormatWriter struct {
	ctx context.Context
	r   *http.Request
	w   http.ResponseWriter
}

func (f *FormatWriter) Response(resp *remote.ReadResponse) error {
	var (
		data []byte
		err  error
	)

	if f.r.Header.Get("Accept") == ContentTypeProtobuf {
		f.w.Header().Set("Content-Type", ContentTypeProtobuf)
		data, err = proto.Marshal(resp)
	} else {
		f.w.Header().Set("Content-Type", ContentTypeJson)
		data, err = json.Marshal(resp)
	}
	if err != nil {
		return err
	}

	if f.r.Header.Get("Accept-Encoding") == ContentEncodingSnappy {
		f.w.Header().Set("Content-Encoding", ContentEncodingSnappy)
		data = snappy.Encode(nil, data)
	}

	_, err = f.w.Write(data)
	return err
}

func GetReadResponse(rs reads.ResultSet) (*remote.ReadResponse, error) {
	resp := &remote.ReadResponse{
		Results: []*remote.QueryResult{{}},
	}
	if rs == nil {
		return resp, nil
	}
	for rs.Next() {
		cur := rs.Cursor()
		if cur == nil {
			// no data for series key + field combination
			continue
		}

		tags := prometheus.RemoveInfluxSystemTags(rs.Tags())
		var unsupportedCursor string
		switch cur := cur.(type) {
		case tsdb.FloatArrayCursor:
			var series *remote.TimeSeries
			for {
				a := cur.Next()
				if a.Len() == 0 {
					break
				}

				// We have some data for this series.
				if series == nil {
					series = &remote.TimeSeries{
						Labels: prometheus.ModelTagsToLabelPairs(tags),
					}
				}

				for i, ts := range a.Timestamps {
					series.Samples = append(series.Samples, &remote.Sample{
						TimestampMs: ts / int64(time.Millisecond),
						Value:       a.Values[i],
					})
				}
			}

			// There was data for the series.
			if series != nil {
				resp.Results[0].Timeseries = append(resp.Results[0].Timeseries, series)
			}
		case tsdb.IntegerArrayCursor:
			var series *remote.TimeSeries
			for {
				a := cur.Next()
				if a.Len() == 0 {
					break
				}

				// We have some data for this series.
				if series == nil {
					series = &remote.TimeSeries{
						Labels: prometheus.ModelTagsToLabelPairs(tags),
					}
				}

				for i, ts := range a.Timestamps {
					series.Samples = append(series.Samples, &remote.Sample{
						TimestampMs: ts / int64(time.Millisecond),
						Value:       float64(a.Values[i]),
					})
				}
			}

			// There was data for the series.
			if series != nil {
				resp.Results[0].Timeseries = append(resp.Results[0].Timeseries, series)
			}
		case tsdb.UnsignedArrayCursor:
			unsupportedCursor = "uint"
		case tsdb.BooleanArrayCursor:
			unsupportedCursor = "bool"
		case tsdb.StringArrayCursor:
			unsupportedCursor = "string"
		default:
			return nil, fmt.Errorf("unreachable: %T", cur)
		}
		cur.Close()

		if len(unsupportedCursor) > 0 {
			return nil, fmt.Errorf("raw can't read cursor, cursor_type: %s, series: %s", unsupportedCursor, tags)
		}
	}

	return resp, nil
}

func GetReadRequest(db, rp, measurement, field, where string) (*datatypes.ReadFilterRequest, error) {
	if db == "" {
		return nil, fmt.Errorf("db is empty")
	}
	if measurement == "" {
		return nil, fmt.Errorf("measurement is empty")
	}
	if field == "" {
		field = "value"
	}
	if where != "" {
		where = fmt.Sprintf("(%s) and ", where)
	}

	src, err := types.MarshalAny(&storage.ReadSource{Database: db, RetentionPolicy: rp})
	if err != nil {
		return nil, err
	}
	// 增加 measurement 和 field
	condition := fmt.Sprintf("%s%s = '%s' and %s = '%s'", where, measurementTagKey, measurement, fieldTagKey, field)
	expr, err := influxql.ParseExpr(condition)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	valuer := influxql.NowValuer{Now: now}
	cond, timeRange, err := influxql.ConditionExpr(expr, &valuer)

	predicate, err := exprToNode(cond)
	if err != nil {
		return nil, err
	}

	// 限制只能查询最近 365 天的数据
	minTimeLimit := time.Now().Add(time.Hour * 24 * 365 * -1)
	if timeRange.MinTimeNano() < minTimeLimit.UnixNano() {
		return nil, fmt.Errorf("start time %s < %s", timeRange.Min.String(), minTimeLimit.String())
	}

	rq := &datatypes.ReadFilterRequest{
		ReadSource: src,
		Range: datatypes.TimestampRange{
			Start: timeRange.MinTimeNano(),
			End:   timeRange.MaxTimeNano(),
		},
		Predicate: predicate,
	}
	return rq, nil
}

func exprToNode(expr influxql.Expr) (*datatypes.Predicate, error) {
	if expr == nil {
		return nil, nil
	}
	var v exprToNodeVisitor
	influxql.Walk(&v, expr)
	if v.Err() != nil {
		return nil, v.Err()
	}

	return &datatypes.Predicate{Root: v.nodes[0]}, nil
}

type exprToNodeVisitor struct {
	nodes []*datatypes.Node
	err   error
}

func (v *exprToNodeVisitor) Err() error {
	return v.err
}

func (v *exprToNodeVisitor) pop() (top *datatypes.Node) {
	if len(v.nodes) < 1 {
		panic("exprToNodeVisitor: stack empty")
	}

	top, v.nodes = v.nodes[len(v.nodes)-1], v.nodes[:len(v.nodes)-1]
	return
}

func (v *exprToNodeVisitor) pop2() (lhs, rhs *datatypes.Node) {
	if len(v.nodes) < 2 {
		panic("exprToNodeVisitor: stack empty")
	}

	rhs = v.nodes[len(v.nodes)-1]
	lhs = v.nodes[len(v.nodes)-2]
	v.nodes = v.nodes[:len(v.nodes)-2]
	return
}

func (v *exprToNodeVisitor) mapOpToComparison(op influxql.Token) datatypes.Node_Comparison {
	switch op {
	case influxql.EQ:
		return datatypes.ComparisonEqual
	case influxql.EQREGEX:
		return datatypes.ComparisonRegex
	case influxql.NEQ:
		return datatypes.ComparisonNotEqual
	case influxql.NEQREGEX:
		return datatypes.ComparisonNotEqual
	case influxql.LT:
		return datatypes.ComparisonLess
	case influxql.LTE:
		return datatypes.ComparisonLessEqual
	case influxql.GT:
		return datatypes.ComparisonGreater
	case influxql.GTE:
		return datatypes.ComparisonGreaterEqual

	default:
		return -1
	}
}

func (v *exprToNodeVisitor) Visit(node influxql.Node) influxql.Visitor {
	switch n := node.(type) {
	case *influxql.BinaryExpr:
		if v.err != nil {
			return nil
		}

		influxql.Walk(v, n.LHS)
		if v.err != nil {
			return nil
		}

		influxql.Walk(v, n.RHS)
		if v.err != nil {
			return nil
		}

		if comp := v.mapOpToComparison(n.Op); comp != -1 {
			lhs, rhs := v.pop2()
			v.nodes = append(v.nodes, &datatypes.Node{
				NodeType: datatypes.NodeTypeComparisonExpression,
				Value:    &datatypes.Node_Comparison_{Comparison: comp},
				Children: []*datatypes.Node{lhs, rhs},
			})
		} else if n.Op == influxql.AND || n.Op == influxql.OR {
			var op datatypes.Node_Logical
			if n.Op == influxql.AND {
				op = datatypes.LogicalAnd
			} else {
				op = datatypes.LogicalOr
			}

			lhs, rhs := v.pop2()
			v.nodes = append(v.nodes, &datatypes.Node{
				NodeType: datatypes.NodeTypeLogicalExpression,
				Value:    &datatypes.Node_Logical_{Logical: op},
				Children: []*datatypes.Node{lhs, rhs},
			})
		} else {
			v.err = fmt.Errorf("unsupported operator, %s", n.Op)
		}

		return nil

	case *influxql.ParenExpr:
		influxql.Walk(v, n.Expr)
		if v.err != nil {
			return nil
		}

		v.nodes = append(v.nodes, &datatypes.Node{
			NodeType: datatypes.NodeTypeParenExpression,
			Children: []*datatypes.Node{v.pop()},
		})
		return nil

	case *influxql.StringLiteral:
		v.nodes = append(v.nodes, &datatypes.Node{
			NodeType: datatypes.NodeTypeLiteral,
			Value:    &datatypes.Node_StringValue{StringValue: n.Val},
		})
		return nil

	case *influxql.NumberLiteral:
		v.nodes = append(v.nodes, &datatypes.Node{
			NodeType: datatypes.NodeTypeLiteral,
			Value:    &datatypes.Node_FloatValue{FloatValue: n.Val},
		})
		return nil

	case *influxql.IntegerLiteral:
		v.nodes = append(v.nodes, &datatypes.Node{
			NodeType: datatypes.NodeTypeLiteral,
			Value:    &datatypes.Node_IntegerValue{IntegerValue: n.Val},
		})
		return nil

	case *influxql.UnsignedLiteral:
		v.nodes = append(v.nodes, &datatypes.Node{
			NodeType: datatypes.NodeTypeLiteral,
			Value:    &datatypes.Node_UnsignedValue{UnsignedValue: n.Val},
		})
		return nil

	case *influxql.VarRef:
		v.nodes = append(v.nodes, &datatypes.Node{
			NodeType: datatypes.NodeTypeTagRef,
			Value:    &datatypes.Node_TagRefValue{TagRefValue: n.Val},
		})
		return nil

	case *influxql.RegexLiteral:
		v.nodes = append(v.nodes, &datatypes.Node{
			NodeType: datatypes.NodeTypeLiteral,
			Value:    &datatypes.Node_RegexValue{RegexValue: n.Val.String()},
		})
		return nil
	default:
		v.err = fmt.Errorf("unsupported expression %T", n)
		return nil
	}
}
