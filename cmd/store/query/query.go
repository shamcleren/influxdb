//go:build ignore
// +build ignore

package query

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gogo/protobuf/types"
	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxdb/services/storage"
	"github.com/influxdata/influxdb/storage/reads/datatypes"
	"github.com/influxdata/influxql"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// Command represents the program execution for "store query".
type Command struct {
	// Standard input/output, overridden for testing.
	Stderr io.Writer
	Stdout io.Writer
	Logger *zap.Logger

	addr            string
	cpuProfile      string
	memProfile      string
	database        string
	retentionPolicy string
	startTime       int64
	endTime         int64
	limit           int64
	slimit          int64
	soffset         int64
	desc            bool
	silent          bool
	expr            string
	agg             string
	groupArg        string
	group           datatypes.ReadRequest_Group
	groupKeys       string
	keys            []string
	hintsArg        string
	hints           datatypes.HintFlags

	aggType datatypes.Aggregate_AggregateType

	// response
	integerSum  int64
	unsignedSum uint64
	floatSum    float64
	pointCount  uint64
}

// NewCommand returns a new instance of Command.
func NewCommand() *Command {
	return &Command{
		Stderr: os.Stderr,
		Stdout: os.Stdout,
	}
}

func parseTime(v string) (int64, error) {
	if s, err := time.Parse(time.RFC3339, v); err == nil {
		return s.UnixNano(), nil
	}

	if i, err := strconv.ParseInt(v, 10, 64); err == nil {
		return i, nil
	}

	return 0, errors.New("invalid time")
}

// Run executes the command.
func (cmd *Command) Run(args ...string) error {
	var start, end string
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	fs.StringVar(&cmd.cpuProfile, "cpuprofile", "", "CPU profile name")
	fs.StringVar(&cmd.memProfile, "memprofile", "", "memory profile name")
	fs.StringVar(&cmd.addr, "addr", ":8082", "the RPC address")
	fs.StringVar(&cmd.database, "database", "", "the database to query")
	fs.StringVar(&cmd.retentionPolicy, "retention", "", "Optional: the retention policy to query")
	fs.StringVar(&start, "start", "", "Optional: the start time to query (RFC3339 format)")
	fs.StringVar(&end, "end", "", "Optional: the end time to query (RFC3339 format)")
	fs.Int64Var(&cmd.slimit, "slimit", 0, "Optional: limit number of series")
	fs.Int64Var(&cmd.soffset, "soffset", 0, "Optional: start offset for series")
	fs.Int64Var(&cmd.limit, "limit", 0, "Optional: limit number of values per series (-1 to return series only)")
	fs.BoolVar(&cmd.desc, "desc", false, "Optional: return results in descending order")
	fs.BoolVar(&cmd.silent, "silent", false, "silence output")
	fs.StringVar(&cmd.expr, "expr", "", "InfluxQL conditional expression")
	fs.StringVar(&cmd.agg, "agg", "", "aggregate functions (sum, count)")
	fs.StringVar(&cmd.groupArg, "group", "none", "group operation (none,all,by,except,disable)")
	fs.StringVar(&cmd.groupKeys, "group-keys", "", "comma-separated list of tags to specify series order")
	fs.StringVar(&cmd.hintsArg, "hints", "none", "comma-separated list of read hints (none,no_points,no_series)")

	fs.SetOutput(cmd.Stdout)
	fs.Usage = func() {
		fmt.Fprintln(cmd.Stdout, "Query via RPC")
		fmt.Fprintf(cmd.Stdout, "Usage: %s query [flags]\n\n", filepath.Base(os.Args[0]))
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return err
	}

	// set defaults
	if start != "" {
		t, err := parseTime(start)
		if err != nil {
			return err
		}
		cmd.startTime = t

	} else {
		cmd.startTime = models.MinNanoTime
	}
	if end != "" {
		t, err := parseTime(end)
		if err != nil {
			return err
		}
		cmd.endTime = t

	} else {
		// set end time to max if it is not set.
		cmd.endTime = models.MaxNanoTime
	}

	if cmd.groupKeys != "" {
		cmd.keys = strings.Split(cmd.groupKeys, ",")
	}

	if err := cmd.validate(); err != nil {
		return err
	}

	conn, err := grpc.Dial(cmd.addr, grpc.WithInsecure())
	if err != nil {
		return err
	}
	defer conn.Close()

	return cmd.query(datatypes.NewStorageClient(conn))
}

func (cmd *Command) validate() error {
	if cmd.database == "" {
		return fmt.Errorf("must specify a database")
	}
	if cmd.startTime != 0 && cmd.endTime != 0 && cmd.endTime < cmd.startTime {
		return fmt.Errorf("end time before start time")
	}

	if cmd.agg != "" {
		agg, ok := datatypes.Aggregate_AggregateType_value[strings.ToUpper(cmd.agg)]
		if !ok {
			return errors.New("invalid aggregate function: " + cmd.agg)
		}
		cmd.aggType = datatypes.Aggregate_AggregateType(agg)
	}

	group, ok := datatypes.ReadRequest_Group_value["GROUP_"+strings.ToUpper(cmd.groupArg)]
	if !ok {
		return errors.New("invalid group type: " + cmd.groupArg)
	}
	cmd.group = datatypes.ReadRequest_Group(group)

	for _, h := range strings.Split(cmd.hintsArg, ",") {
		cmd.hints |= datatypes.HintFlags(datatypes.ReadRequest_HintFlags_value["HINT_"+strings.ToUpper(h)])
	}

	return nil
}

func (cmd *Command) query(c datatypes.StorageClient) error {
	src := storage.ReadSource{
		Database:        cmd.database,
		RetentionPolicy: cmd.retentionPolicy,
	}

	var req datatypes.ReadRequest
	if any, err := types.MarshalAny(&src); err != nil {
		return err
	} else {
		req.ReadSource = any
	}
	req.TimestampRange.Start = cmd.startTime
	req.TimestampRange.End = cmd.endTime
	req.SeriesLimit = cmd.slimit
	req.SeriesOffset = cmd.soffset
	req.PointsLimit = cmd.limit
	req.Descending = cmd.desc
	req.Group = cmd.group
	req.GroupKeys = cmd.keys
	req.Hints = cmd.hints

	if cmd.aggType != datatypes.AggregateTypeNone {
		req.Aggregate = &datatypes.Aggregate{Type: cmd.aggType}
	}

	if cmd.expr != "" {
		expr, err := influxql.ParseExpr(cmd.expr)
		if err != nil {
			return nil
		}
		fmt.Fprintln(cmd.Stdout, expr)
		var v exprToNodeVisitor
		influxql.Walk(&v, expr)
		if v.Err() != nil {
			return v.Err()
		}

		req.Predicate = &datatypes.Predicate{Root: v.nodes[0]}
	}

	stream, err := c.Read(context.Background(), &req)
	if err != nil {
		fmt.Fprintln(cmd.Stdout, err)
		return err
	}

	wr := bufio.NewWriter(os.Stdout)

	now := time.Now()
	defer func() {
		dur := time.Since(now)
		fmt.Fprintf(cmd.Stdout, "time: %v\n", dur)
	}()

	for {
		var rep datatypes.ReadResponse

		if err = stream.RecvMsg(&rep); err != nil {
			if err == io.EOF {
				break
			}

			return err
		}

		if cmd.silent {
			cmd.processFramesSilent(rep.Frames)
		} else {
			cmd.processFrames(wr, rep.Frames)
		}
	}

	fmt.Fprintln(cmd.Stdout)
	fmt.Fprint(cmd.Stdout, "points(count): ", cmd.pointCount, ", sum(int64): ", cmd.integerSum, ", sum(uint64): ", cmd.unsignedSum, ", sum(float64): ", cmd.floatSum, "\n")

	return nil
}

func (cmd *Command) processFramesSilent(frames []datatypes.ReadResponse_Frame) {
	for _, frame := range frames {
		switch f := frame.Data.(type) {
		case *datatypes.ReadResponse_Frame_IntegerPoints:
			for _, v := range f.IntegerPoints.Values {
				cmd.integerSum += v
			}
			cmd.pointCount += uint64(len(f.IntegerPoints.Values))

		case *datatypes.ReadResponse_Frame_UnsignedPoints:
			for _, v := range f.UnsignedPoints.Values {
				cmd.unsignedSum += v
			}
			cmd.pointCount += uint64(len(f.UnsignedPoints.Values))

		case *datatypes.ReadResponse_Frame_FloatPoints:
			for _, v := range f.FloatPoints.Values {
				cmd.floatSum += v
			}
			cmd.pointCount += uint64(len(f.FloatPoints.Values))

		case *datatypes.ReadResponse_Frame_StringPoints:
			cmd.pointCount += uint64(len(f.StringPoints.Values))

		case *datatypes.ReadResponse_Frame_BooleanPoints:
			cmd.pointCount += uint64(len(f.BooleanPoints.Values))
		}
	}
}

func printByteSlice(wr *bufio.Writer, v [][]byte) {
	wr.WriteString("[\033[36m")
	first := true
	for _, t := range v {
		if !first {
			wr.WriteByte(',')
		} else {
			first = false
		}
		wr.Write(t)
	}
	wr.WriteString("\033[0m]\n")
}

func (cmd *Command) processFrames(wr *bufio.Writer, frames []datatypes.ReadResponse_Frame) {
	var buf [1024]byte
	var line []byte

	for _, frame := range frames {
		switch f := frame.Data.(type) {
		case *datatypes.ReadResponse_Frame_Group:
			g := f.Group
			wr.WriteString("partition values")
			printByteSlice(wr, g.PartitionKeyVals)
			wr.WriteString("group keys")
			printByteSlice(wr, g.TagKeys)
			wr.Flush()

		case *datatypes.ReadResponse_Frame_Series:
			s := f.Series
			wr.WriteString("\033[36m")
			first := true
			for _, t := range s.Tags {
				if !first {
					wr.WriteByte(',')
				} else {
					first = false
				}
				wr.Write(t.Key)
				wr.WriteByte(':')
				wr.Write(t.Value)
			}
			wr.WriteString("\033[0m\n")
			wr.Flush()

		case *datatypes.ReadResponse_Frame_IntegerPoints:
			p := f.IntegerPoints
			for i := 0; i < len(p.Timestamps); i++ {
				line = buf[:0]
				wr.Write(strconv.AppendInt(line, p.Timestamps[i], 10))
				wr.WriteByte(' ')

				line = buf[:0]
				wr.Write(strconv.AppendInt(line, p.Values[i], 10))
				wr.WriteString("\n")
				wr.Flush()

				cmd.integerSum += p.Values[i]
			}
			cmd.pointCount += uint64(len(f.IntegerPoints.Values))

		case *datatypes.ReadResponse_Frame_UnsignedPoints:
			p := f.UnsignedPoints
			for i := 0; i < len(p.Timestamps); i++ {
				line = buf[:0]
				wr.Write(strconv.AppendInt(line, p.Timestamps[i], 10))
				wr.WriteByte(' ')

				line = buf[:0]
				wr.Write(strconv.AppendUint(line, p.Values[i], 10))
				wr.WriteString("\n")
				wr.Flush()

				cmd.unsignedSum += p.Values[i]
			}
			cmd.pointCount += uint64(len(f.UnsignedPoints.Values))

		case *datatypes.ReadResponse_Frame_FloatPoints:
			p := f.FloatPoints
			for i := 0; i < len(p.Timestamps); i++ {
				line = buf[:0]
				wr.Write(strconv.AppendInt(line, p.Timestamps[i], 10))
				wr.WriteByte(' ')

				line = buf[:0]
				wr.Write(strconv.AppendFloat(line, p.Values[i], 'f', 10, 64))
				wr.WriteString("\n")
				wr.Flush()

				cmd.floatSum += p.Values[i]
			}
			cmd.pointCount += uint64(len(f.FloatPoints.Values))

		case *datatypes.ReadResponse_Frame_StringPoints:
			p := f.StringPoints
			for i := 0; i < len(p.Timestamps); i++ {
				line = buf[:0]
				wr.Write(strconv.AppendInt(line, p.Timestamps[i], 10))
				wr.WriteByte(' ')

				wr.WriteString(p.Values[i])
				wr.WriteString("\n")
				wr.Flush()
			}
			cmd.pointCount += uint64(len(f.StringPoints.Values))

		case *datatypes.ReadResponse_Frame_BooleanPoints:
			p := f.BooleanPoints
			for i := 0; i < len(p.Timestamps); i++ {
				line = buf[:0]
				wr.Write(strconv.AppendInt(line, p.Timestamps[i], 10))
				wr.WriteByte(' ')

				if p.Values[i] {
					wr.WriteString("true")
				} else {
					wr.WriteString("false")
				}
				wr.WriteString("\n")
				wr.Flush()
			}
			cmd.pointCount += uint64(len(f.BooleanPoints.Values))
		}
	}
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

func mapOpToComparison(op influxql.Token) datatypes.Node_Comparison {
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

		if comp := mapOpToComparison(n.Op); comp != -1 {
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
