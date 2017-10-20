package v3

import (
	"bytes"
	"fmt"
	"math/bits"

	"github.com/cockroachdb/cockroach/pkg/sql/parser"
)

// queryState holds per-query state such as the tables referenced by the query
// and the mapping from table name to the column index for those tables columns
// within the query.
type queryState struct {
	catalog map[string]*table
	tables  map[string]bitmapIndex
	nextVar bitmapIndex
	data    []interface{}
}

func (s *queryState) addData(d interface{}) int32 {
	s.data = append(s.data, d)
	return int32(len(s.data))
}

func (s *queryState) getData(idx int32) interface{} {
	if idx == 0 {
		return nil
	}
	return s.data[idx-1]
}

type columnProps struct {
	name   string
	tables []string
	index  bitmapIndex
	// TODO(peter): value constraints.
}

func (c columnProps) hasColumn(tableName, colName string) bool {
	if colName != c.name {
		return false
	}
	if tableName == "" {
		return true
	}
	return c.hasTable(tableName)
}

func (c columnProps) hasTable(tableName string) bool {
	for _, t := range c.tables {
		if t == tableName {
			return true
		}
	}
	return false
}

func (c columnProps) resolvedName(tableName string) *parser.ColumnItem {
	if tableName == "" {
		if len(c.tables) > 0 {
			tableName = c.tables[0]
		}
	}
	return &parser.ColumnItem{
		TableName: parser.TableName{
			TableName:               parser.Name(tableName),
			DBNameOriginallyOmitted: true,
		},
		ColumnName: parser.Name(c.name),
	}
}

func (c columnProps) newVariableExpr(tableName string, props *logicalProps) *expr {
	e := &expr{
		op:        variableOp,
		dataIndex: props.state.addData(c.resolvedName(tableName)),
		props:     props,
	}
	e.setVarIndex(c.index)
	e.inputVars.set(c.index)
	e.updateProps()
	return e
}

// TODO(peter): Track foreign key constraints for join elimination. Consider
// the schema:
//
//   CREATE TABLE departments (
//     dept_id INT PRIMARY KEY,
//     name STRING
//   );
//
//   CREATE TABLE employees (
//     emp_id INT PRIMARY KEY,
//     dept_id INT REFERENCES d (dept_id),
//     name STRING,
//     salary INT
//   );
//
// And the query:
//
//   SELECT e.name, e.salary
//   FROM employees e, departments d
//   WHERE e.dept_id = d.dept_id
//
// The foreign key constraint specifies that employees.dept_id must match a
// value in departments.dept_id or be NULL. Because departments.dept_id is NOT
// NULL (due to being part of the primary key), we know the only rows from
// employees that will not be in the join are those with a NULL dept_id. So we
// can transform the query into:
//
//   SELECT e.name, e.salary
//   FROM employees e
//   WHERE e.dept_id IS NOT NULL
//
// Foreign keys are represented by src and dest bitmaps.
//
// type foreignKeyProps struct {
//   src  bitmap
//   dest bitmap
// }
//
// Note that this can be seen as a generalization of candidateKeys where he
// dependent vars are explicit instead of being implicit in the columns.

type logicalProps struct {
	columns []columnProps
	// Bitmap indicating which output columns cannot be NULL. The NULL-ability of
	// columns flows from the inputs and can also be derived from filters that
	// are NULL-intolerant.
	notNullCols bitmap
	// Required output vars is the set of output variables that parent expression
	// requires. This must be a subset of logicalProperties.outputVars.
	requiredOutputVars bitmap

	// TODO(peter): Bitmap indicating which output columns are constant.
	// constCols bitmap

	// A column set is a key if no two rows are equal after projection onto that
	// set. A requirement for a column set to be a key is for no columns in the
	// set to be NULL-able. This requirement stems from the property of NULL
	// where NULL != NULL. The simplest example of a key is the primary key for a
	// table (recall that all of the columns of the primary key are defined to be
	// NOT NULL).
	//
	// A candidate key is a set of columns where no two rows containing non-NULL
	// values are equal after projection onto that set. A UNIQUE index on a table
	// is a candidate key and possibly a key if all of the columns are NOT
	// NULL. A candidate key is a key if "(candidateKeys[i] & notNullColumns) ==
	// candidateKeys[i]".
	candidateKeys []bitmap
	// The global query state.
	state *queryState
}

func (p *logicalProps) String() string {
	var buf bytes.Buffer
	p.format(&buf, 0)
	return buf.String()
}

func (p *logicalProps) format(buf *bytes.Buffer, level int) {
	indent := spaces[:2*level]
	fmt.Fprintf(buf, "%scolumns:", indent)
	for _, col := range p.columns {
		buf.WriteString(" ")
		if p.requiredOutputVars.get(col.index) {
			buf.WriteString("+")
		}
		if tables := col.tables; len(tables) > 1 {
			buf.WriteString("{")
			for j, table := range tables {
				if j > 0 {
					buf.WriteString(",")
				}
				buf.WriteString(table)
			}
			buf.WriteString("}")
		} else if len(tables) == 1 {
			buf.WriteString(tables[0])
		}
		buf.WriteString(".")
		buf.WriteString(col.name)
		buf.WriteString(":")
		fmt.Fprintf(buf, "%d", col.index)
		if p.notNullCols.get(col.index) {
			buf.WriteString("*")
		}
	}
	buf.WriteString("\n")
	for _, key := range p.candidateKeys {
		var prefix string
		if (key & p.notNullCols) != key {
			prefix = "weak "
		}
		fmt.Fprintf(buf, "%s%skey: %s", indent, prefix, key)
		buf.WriteString("\n")
	}
}

func (p *logicalProps) newColumnExpr(name string) *expr {
	for _, col := range p.columns {
		if col.name == name {
			return col.newVariableExpr(col.tables[0], p)
		}
	}
	return nil
}

// Add additional not-NULL columns based on the filtering expressions.
func (p *logicalProps) applyFilters(filters []*expr) {
	for _, filter := range filters {
		// TODO(peter): !isNullTolerant(filter)
		for v := filter.inputVars; v != 0; {
			i := uint(bits.TrailingZeros64(uint64(v)))
			v &^= 1 << i
			p.notNullCols |= 1 << i
		}
	}
}

func (p *logicalProps) outputVars() bitmap {
	var b bitmap
	for _, col := range p.columns {
		b.set(col.index)
	}
	return b
}

func concatLogicalProperties(left, right *logicalProps) *logicalProps {
	t := &logicalProps{
		columns: make([]columnProps, len(left.columns)+len(right.columns)),
		state:   left.state,
	}
	copy(t.columns[:], left.columns)
	copy(t.columns[len(left.columns):], right.columns)
	return t
}
