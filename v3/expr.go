package v3

import (
	"bytes"
	"fmt"
)

// expr is a unified interface for both relational and scalar expressions in a
// query. Expressions have optional inputs and filters. Specific operators also
// maintain additional expressions in the aux1 and aux2 slices. In particular,
// projectOp stores the projection expressions in aux1, groupByOp stores the
// grouping expressions in aux1 and the aggregations in aux2 and orderByOp
// stores the sorting expressions in aux2.
//
// Expressions contain a pointer to their logical properties. For scalar
// expressions, the logical properties points to the context in which the
// scalar is defined.
//
// Every unique column and every projection (that is more than just a pass
// through of a variable) is given a variable index with the query. The
// variable indexes are global to the query (see queryState). For example,
// consider the query:
//
//   SELECT x FROM a WHERE y > 0
//
// There are 2 variables in the above query: x and y. During name resolution,
// the above query becomes:
//
//   SELECT [0] FROM a WHERE [1] > 0
//   -- [0] -> x
//   -- [1] -> y
//
// This is akin to the way parser.IndexedVar works except that we're taking
// care to make the indexes unique across the entire statement. Because each of
// the relational expression nodes maintains a bitmap of the variables it
// outputs we can quickly determine if a scalar expression can be handled using
// bitmap intersection.
//
// For scalar expressions the input variables bitmap allows an easy
// determination of whether the expression is constant (the bitmap is empty)
// and, if not, which variables it uses. Predicate push down can use this
// bitmap to quickly determine whether a filter can be pushed below a
// relational operator.
//
// Relational expressions are composed of inputs, optional filters and optional
// auxiliary expressions. The output columns are derived by the operator from
// the inputs and stored in expr.props.columns.
//
//   +---------+---------+-------+--------+
//   |  out 0  |  out 1  |  ...  |  out N |
//   +---------+---------+-------+--------+
//   |             filters                |
//   +------------------------------------+
//   |        operator (aux1, aux)        |
//   +---------+---------+-------+--------+
//   |  in 0   |  in 1   |  ...  |  in N  |
//   +---------+---------+-------+--------+
//
// A query is composed of a tree of relational expressions. For example, a
// simple join might look like:
//
//   +-----------+
//   | join a, b |
//   +-----------+
//      |     |
//      |     |   +--------+
//      |     +---| scan b |
//      |         +--------+
//      |
//      |    +--------+
//      +----| scan a |
//           +--------+
//
// The output variables of each expression need to be compatible with input
// columns of its parent expression. Notice that the input variables of an
// expression constrain what output variables we need from the children. That
// constrain can be expressed by bitmap intersection. For example, consider the
// query:
//
//   SELECT a.x FROM a JOIN b USING (x)
//
// The only column from "a" that is required is "x". This is expressed in the
// code by the inputs required by the projection ("a.x") and the inputs
// required by the join condition (also "a.x").
type expr struct {
	// NB: op, projectCount and filterCount are placed next to each other in
	// order to reduce space wastage due to padding.
	op operator
	// The inputs, projections and filters are all stored in the children slice
	// to minimize overhead. auxMask indicates which of these auxiliary
	// expressions is present.
	auxMask uint16
	// The index of a data item (interface{}) for use by this expresssion. The
	// data is accessible via expr.props.state.getData(). Used by scalar
	// expressions to store additional info, such as the column name of a
	// variable or the value of a constant.
	dataIndex int32
	// The input and output bitmaps specified required inputs and generated
	// outputs. The indexes refer to queryState.columns which is constructed on a
	// per-query basis by the columns required by filters, join conditions, and
	// projections and the new columns generated by projections.
	inputVars  bitmap
	outputVars bitmap
	children   []*expr
	props      *logicalProps
}

func (e *expr) String() string {
	var buf bytes.Buffer
	e.format(&buf, 0)
	return buf.String()
}

func (e *expr) format(buf *bytes.Buffer, level int) {
	e.info().format(e, buf, level)
}

func (e *expr) formatVars(buf *bytes.Buffer) {
	if e.inputVars != 0 || e.outputVars != 0 {
		buf.WriteString(" [")
		sep := ""
		if e.inputVars != 0 {
			fmt.Fprintf(buf, "in=%s", e.inputVars)
			sep = " "
		}
		if e.outputVars != 0 {
			sep = " "
			fmt.Fprintf(buf, "%sout=%s", sep, e.outputVars)
		}
		buf.WriteString("]")
	}
}

func formatExprs(buf *bytes.Buffer, title string, exprs []*expr, level int) {
	if len(exprs) > 0 {
		indent := spaces[:2*level]
		fmt.Fprintf(buf, "%s  %s:\n", indent, title)
		for _, e := range exprs {
			e.format(buf, level+2)
		}
	}
}

func (e *expr) clone() *expr {
	t := *e
	t.children = make([]*expr, len(e.children))
	copy(t.children, e.children)
	return &t
}

func (e *expr) inputCount() int {
	return len(e.children) - (e.filterPresent() + e.aux1Present() + e.aux2Present())
}

func (e *expr) inputs() []*expr {
	return e.children[:e.inputCount()]
}

const (
	auxFilterBit = iota
	aux1Bit
	aux2Bit
)

func (e *expr) filterPresent() int {
	return int((e.auxMask >> auxFilterBit) & 1)
}

func (e *expr) filters() []*expr {
	if e.filterPresent() == 0 {
		return nil
	}
	i := len(e.children) - 1
	f := e.children[i:]
	if f[0].op == andOp {
		return f[0].children
	}
	return f
}

func (e *expr) addFilter(f *expr) {
	// Recursively flatten AND expressions when adding them as a filter. The
	// filters for an expression are implicitly AND'ed together (i.e. they are in
	// conjunctive normal form).
	if f.op == andOp {
		for _, input := range f.inputs() {
			e.addFilter(input)
		}
		return
	}

	if e.filterPresent() == 0 {
		e.auxMask |= 1 << auxFilterBit
		e.children = append(e.children, f)
	} else {
		i := len(e.children) - 1
		if t := e.children[i]; t.op != andOp {
			e.children[i] = &expr{
				op:       andOp,
				children: []*expr{t, f},
				props:    t.props,
			}
		} else {
			t.children = append(t.children, f)
		}
	}
}

func (e *expr) removeFilters() {
	filterStart := len(e.children) - e.filterPresent()
	e.children = e.children[:filterStart]
	e.auxMask &^= 1 << auxFilterBit
}

func (e *expr) aux1Present() int {
	return int((e.auxMask >> aux1Bit) & 1)
}

func (e *expr) aux1Index() int {
	if e.aux1Present() == 0 {
		return -1
	}
	return len(e.children) - 1 - e.filterPresent()
}

func (e *expr) aux1() []*expr {
	i := e.aux1Index()
	if i < 0 {
		return nil
	}
	return e.children[i].children
}

func (e *expr) addAux1(exprs []*expr) {
	if e.aux1Present() == 0 {
		e.auxMask |= 1 << aux1Bit
		e.children = append(e.children, nil)
		i := e.aux1Index()
		copy(e.children[i+1:], e.children[i:])
		e.children[i] = &expr{
			op:       andOp,
			children: exprs,
			props:    e.props,
		}
	} else {
		i := e.aux1Index()
		aux1 := e.children[i]
		aux1.children = append(aux1.children, exprs...)
	}
}

func (e *expr) aux2Present() int {
	return int((e.auxMask >> aux2Bit) & 1)
}

func (e *expr) aux2Index() int {
	if e.aux2Present() == 0 {
		return -1
	}
	return len(e.children) - 1 - int(e.filterPresent()+e.aux1Present())
}

func (e *expr) aux2() []*expr {
	i := e.aux2Index()
	if i < 0 {
		return nil
	}
	return e.children[i].children
}

func (e *expr) addAux2(exprs []*expr) {
	if e.aux2Present() == 0 {
		e.auxMask |= 1 << aux2Bit
		e.children = append(e.children, nil)
		i := e.aux2Index()
		copy(e.children[i+1:], e.children[i:])
		e.children[i] = &expr{
			op:       andOp,
			children: exprs,
			props:    e.props,
		}
	} else {
		i := e.aux2Index()
		aux2 := e.children[i]
		aux2.children = append(aux2.children, exprs...)
	}
}

func (e *expr) projections() []*expr {
	if e.op != projectOp {
		fatalf("%s: invalid use of projections", e.op)
	}
	return e.aux1()
}

func (e *expr) addProjections(exprs []*expr) {
	if e.op != projectOp {
		fatalf("%s: invalid use of projections", e.op)
	}
	e.addAux1(exprs)
}

func (e *expr) groupings() []*expr {
	if e.op != groupByOp {
		fatalf("%s: invalid use of groupings", e.op)
	}
	return e.aux1()
}

func (e *expr) addGroupings(exprs []*expr) {
	if e.op != groupByOp {
		fatalf("%s: invalid use of groupings", e.op)
	}
	e.addAux1(exprs)
}

func (e *expr) aggregations() []*expr {
	if e.op != groupByOp {
		fatalf("%s: invalid use of aggregations", e.op)
	}
	return e.aux2()
}

func (e *expr) addAggregations(exprs []*expr) {
	if e.op != groupByOp {
		fatalf("%s: invalid use of aggregations", e.op)
	}
	e.addAux2(exprs)
}

func (e *expr) info() *operatorInfo {
	return &operatorTab[e.op]
}

func (e *expr) updateProperties() {
	e.info().updateProperties(e)
}
