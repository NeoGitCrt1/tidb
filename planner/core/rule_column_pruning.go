// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package core

import (
	"context"

	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/expression/aggregation"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/parser/ast"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/parser/mysql"
	"github.com/pingcap/tidb/planner/util"
)

type columnPruner struct {
}

func (s *columnPruner) optimize(ctx context.Context, lp LogicalPlan) (LogicalPlan, error) {
	err := lp.PruneColumns(lp.Schema().Columns)
	return lp, err
}

// ExprsHasSideEffects checks if any of the expressions has side effects.
func ExprsHasSideEffects(exprs []expression.Expression) bool {
	for _, expr := range exprs {
		if exprHasSetVarOrSleep(expr) {
			return true
		}
	}
	return false
}

// exprHasSetVarOrSleep checks if the expression has SetVar function or Sleep function.
func exprHasSetVarOrSleep(expr expression.Expression) bool {
	scalaFunc, isScalaFunc := expr.(*expression.ScalarFunction)
	if !isScalaFunc {
		return false
	}
	if scalaFunc.FuncName.L == ast.SetVar || scalaFunc.FuncName.L == ast.Sleep {
		return true
	}
	for _, arg := range scalaFunc.GetArgs() {
		if exprHasSetVarOrSleep(arg) {
			return true
		}
	}
	return false
}

// PruneColumns implements LogicalPlan interface.
// If any expression has SetVar function or Sleep function, we do not prune it.
func (p *LogicalProjection) PruneColumns(parentUsedCols []*expression.Column) error {
	child := p.children[0]
	used := expression.GetUsedList(parentUsedCols, p.schema)

	for i := len(used) - 1; i >= 0; i-- {
		if !used[i] && !exprHasSetVarOrSleep(p.Exprs[i]) {
			p.schema.Columns = append(p.schema.Columns[:i], p.schema.Columns[i+1:]...)
			p.Exprs = append(p.Exprs[:i], p.Exprs[i+1:]...)
		}
	}
	selfUsedCols := make([]*expression.Column, 0, len(p.Exprs))
	selfUsedCols = expression.ExtractColumnsFromExpressions(selfUsedCols, p.Exprs, nil)
	return child.PruneColumns(selfUsedCols)
}

// PruneColumns implements LogicalPlan interface.
func (p *LogicalSelection) PruneColumns(parentUsedCols []*expression.Column) error {
	child := p.children[0]
	parentUsedCols = expression.ExtractColumnsFromExpressions(parentUsedCols, p.Conditions, nil)
	return child.PruneColumns(parentUsedCols)
}

// PruneColumns implements LogicalPlan interface.
func (la *LogicalAggregation) PruneColumns(parentUsedCols []*expression.Column) error {
	child := la.children[0]
	used := expression.GetUsedList(parentUsedCols, la.Schema())

	allFirstRow := true
	allRemainFirstRow := true
	for i := len(used) - 1; i >= 0; i-- {
		if la.AggFuncs[i].Name != ast.AggFuncFirstRow {
			allFirstRow = false
		}
		if !used[i] && !ExprsHasSideEffects(la.AggFuncs[i].Args) {
			la.schema.Columns = append(la.schema.Columns[:i], la.schema.Columns[i+1:]...)
			la.AggFuncs = append(la.AggFuncs[:i], la.AggFuncs[i+1:]...)
		} else if la.AggFuncs[i].Name != ast.AggFuncFirstRow {
			allRemainFirstRow = false
		}
	}
	var selfUsedCols []*expression.Column
	for _, aggrFunc := range la.AggFuncs {
		selfUsedCols = expression.ExtractColumnsFromExpressions(selfUsedCols, aggrFunc.Args, nil)

		var cols []*expression.Column
		aggrFunc.OrderByItems, cols = pruneByItems(aggrFunc.OrderByItems)
		selfUsedCols = append(selfUsedCols, cols...)
	}
	if len(la.AggFuncs) == 0 || (!allFirstRow && allRemainFirstRow) {
		// If all the aggregate functions are pruned, we should add an aggregate function to maintain the info of row numbers.
		// For all the aggregate functions except `first_row`, if we have an empty table defined as t(a,b),
		// `select agg(a) from t` would always return one row, while `select agg(a) from t group by b` would return empty.
		// For `first_row` which is only used internally by tidb, `first_row(a)` would always return empty for empty input now.
		var err error
		var newAgg *aggregation.AggFuncDesc
		if allFirstRow {
			newAgg, err = aggregation.NewAggFuncDesc(la.ctx, ast.AggFuncFirstRow, []expression.Expression{expression.NewOne()}, false)
		} else {
			newAgg, err = aggregation.NewAggFuncDesc(la.ctx, ast.AggFuncCount, []expression.Expression{expression.NewOne()}, false)
		}
		if err != nil {
			return err
		}
		la.AggFuncs = append(la.AggFuncs, newAgg)
		col := &expression.Column{
			UniqueID: la.ctx.GetSessionVars().AllocPlanColumnID(),
			RetType:  newAgg.RetTp,
		}
		la.schema.Columns = append(la.schema.Columns, col)
	}

	if len(la.GroupByItems) > 0 {
		for i := len(la.GroupByItems) - 1; i >= 0; i-- {
			cols := expression.ExtractColumns(la.GroupByItems[i])
			if len(cols) == 0 && !exprHasSetVarOrSleep(la.GroupByItems[i]) {
				la.GroupByItems = append(la.GroupByItems[:i], la.GroupByItems[i+1:]...)
			} else {
				selfUsedCols = append(selfUsedCols, cols...)
			}
		}
		// If all the group by items are pruned, we should add a constant 1 to keep the correctness.
		// Because `select count(*) from t` is different from `select count(*) from t group by 1`.
		if len(la.GroupByItems) == 0 {
			la.GroupByItems = []expression.Expression{expression.NewOne()}
		}
	}
	return child.PruneColumns(selfUsedCols)
}

func pruneByItems(old []*util.ByItems) (new []*util.ByItems, parentUsedCols []*expression.Column) {
	new = make([]*util.ByItems, 0, len(old))
	seen := make(map[string]struct{}, len(old))
	for _, byItem := range old {
		hash := string(byItem.Expr.HashCode(nil))
		_, hashMatch := seen[hash]
		seen[hash] = struct{}{}
		cols := expression.ExtractColumns(byItem.Expr)
		if hashMatch {
			// do nothing, should be filtered
		} else if len(cols) == 0 {
			if !expression.IsRuntimeConstExpr(byItem.Expr) {
				new = append(new, byItem)
			}
		} else if byItem.Expr.GetType().Tp == mysql.TypeNull {
			// do nothing, should be filtered
		} else {
			parentUsedCols = append(parentUsedCols, cols...)
			new = append(new, byItem)
		}
	}
	return
}

// PruneColumns implements LogicalPlan interface.
// If any expression can view as a constant in execution stage, such as correlated column, constant,
// we do prune them. Note that we can't prune the expressions contain non-deterministic functions, such as rand().
func (ls *LogicalSort) PruneColumns(parentUsedCols []*expression.Column) error {
	child := ls.children[0]
	var cols []*expression.Column
	ls.ByItems, cols = pruneByItems(ls.ByItems)
	parentUsedCols = append(parentUsedCols, cols...)
	return child.PruneColumns(parentUsedCols)
}

// PruneColumns implements LogicalPlan interface.
// If any expression can view as a constant in execution stage, such as correlated column, constant,
// we do prune them. Note that we can't prune the expressions contain non-deterministic functions, such as rand().
func (lt *LogicalTopN) PruneColumns(parentUsedCols []*expression.Column) error {
	child := lt.children[0]
	var cols []*expression.Column
	lt.ByItems, cols = pruneByItems(lt.ByItems)
	parentUsedCols = append(parentUsedCols, cols...)
	return child.PruneColumns(parentUsedCols)
}

// PruneColumns implements LogicalPlan interface.
func (p *LogicalUnionAll) PruneColumns(parentUsedCols []*expression.Column) error {
	used := expression.GetUsedList(parentUsedCols, p.schema)
	hasBeenUsed := false
	for i := range used {
		hasBeenUsed = hasBeenUsed || used[i]
		if hasBeenUsed {
			break
		}
	}
	if !hasBeenUsed {
		parentUsedCols = make([]*expression.Column, len(p.schema.Columns))
		copy(parentUsedCols, p.schema.Columns)
	}
	for _, child := range p.Children() {
		err := child.PruneColumns(parentUsedCols)
		if err != nil {
			return err
		}
	}

	if hasBeenUsed {
		// keep the schema of LogicalUnionAll same as its children's
		used := expression.GetUsedList(p.children[0].Schema().Columns, p.schema)
		for i := len(used) - 1; i >= 0; i-- {
			if !used[i] {
				p.schema.Columns = append(p.schema.Columns[:i], p.schema.Columns[i+1:]...)
			}
		}
	}
	return nil
}

// PruneColumns implements LogicalPlan interface.
func (p *LogicalUnionScan) PruneColumns(parentUsedCols []*expression.Column) error {
	for i := 0; i < p.handleCols.NumCols(); i++ {
		parentUsedCols = append(parentUsedCols, p.handleCols.GetCol(i))
	}
	condCols := expression.ExtractColumnsFromExpressions(nil, p.conditions, nil)
	parentUsedCols = append(parentUsedCols, condCols...)
	return p.children[0].PruneColumns(parentUsedCols)
}

// PruneColumns implements LogicalPlan interface.
func (ds *DataSource) PruneColumns(parentUsedCols []*expression.Column) error {
	used := expression.GetUsedList(parentUsedCols, ds.schema)

	exprCols := expression.ExtractColumnsFromExpressions(nil, ds.allConds, nil)
	exprUsed := expression.GetUsedList(exprCols, ds.schema)

	originSchemaColumns := ds.schema.Columns
	originColumns := ds.Columns
	for i := len(used) - 1; i >= 0; i-- {
		if !used[i] && !exprUsed[i] {
			ds.schema.Columns = append(ds.schema.Columns[:i], ds.schema.Columns[i+1:]...)
			ds.Columns = append(ds.Columns[:i], ds.Columns[i+1:]...)
		}
	}
	// For SQL like `select 1 from t`, tikv's response will be empty if no column is in schema.
	// So we'll force to push one if schema doesn't have any column.
	if ds.schema.Len() == 0 {
		var handleCol *expression.Column
		var handleColInfo *model.ColumnInfo
		if ds.table.Type().IsClusterTable() && len(originColumns) > 0 {
			// use the first line.
			handleCol = originSchemaColumns[0]
			handleColInfo = originColumns[0]
		} else {
			if ds.handleCols != nil {
				handleCol = ds.handleCols.GetCol(0)
				handleColInfo = handleCol.ToInfo()
			} else {
				handleCol = ds.newExtraHandleSchemaCol()
				handleColInfo = model.NewExtraHandleColInfo()
			}
		}
		ds.Columns = append(ds.Columns, handleColInfo)
		ds.schema.Append(handleCol)
	}
	if ds.handleCols != nil && ds.handleCols.IsInt() && ds.schema.ColumnIndex(ds.handleCols.GetCol(0)) == -1 {
		ds.handleCols = nil
	}
	return nil
}

// PruneColumns implements LogicalPlan interface.
func (p *LogicalMemTable) PruneColumns(parentUsedCols []*expression.Column) error {
	switch p.TableInfo.Name.O {
	case infoschema.TableStatementsSummary,
		infoschema.TableStatementsSummaryHistory,
		infoschema.TableSlowQuery,
		infoschema.ClusterTableStatementsSummary,
		infoschema.ClusterTableStatementsSummaryHistory,
		infoschema.ClusterTableSlowLog,
		infoschema.TableTiDBTrx,
		infoschema.ClusterTableTiDBTrx,
		infoschema.TableDataLockWaits,
		infoschema.TableDeadlocks,
		infoschema.ClusterTableDeadlocks:
	default:
		return nil
	}
	used := expression.GetUsedList(parentUsedCols, p.schema)
	for i := len(used) - 1; i >= 0; i-- {
		if !used[i] && p.schema.Len() > 1 {
			p.schema.Columns = append(p.schema.Columns[:i], p.schema.Columns[i+1:]...)
			p.names = append(p.names[:i], p.names[i+1:]...)
			p.Columns = append(p.Columns[:i], p.Columns[i+1:]...)
		}
	}
	return nil
}

// PruneColumns implements LogicalPlan interface.
func (p *LogicalTableDual) PruneColumns(parentUsedCols []*expression.Column) error {
	used := expression.GetUsedList(parentUsedCols, p.Schema())

	for i := len(used) - 1; i >= 0; i-- {
		if !used[i] {
			p.schema.Columns = append(p.schema.Columns[:i], p.schema.Columns[i+1:]...)
		}
	}
	return nil
}

func (p *LogicalJoin) extractUsedCols(parentUsedCols []*expression.Column) (leftCols []*expression.Column, rightCols []*expression.Column) {
	for _, eqCond := range p.EqualConditions {
		parentUsedCols = append(parentUsedCols, expression.ExtractColumns(eqCond)...)
	}
	for _, leftCond := range p.LeftConditions {
		parentUsedCols = append(parentUsedCols, expression.ExtractColumns(leftCond)...)
	}
	for _, rightCond := range p.RightConditions {
		parentUsedCols = append(parentUsedCols, expression.ExtractColumns(rightCond)...)
	}
	for _, otherCond := range p.OtherConditions {
		parentUsedCols = append(parentUsedCols, expression.ExtractColumns(otherCond)...)
	}
	lChild := p.children[0]
	rChild := p.children[1]
	for _, col := range parentUsedCols {
		if lChild.Schema().Contains(col) {
			leftCols = append(leftCols, col)
		} else if rChild.Schema().Contains(col) {
			rightCols = append(rightCols, col)
		}
	}
	return leftCols, rightCols
}

func (p *LogicalJoin) mergeSchema() {
	p.schema = buildLogicalJoinSchema(p.JoinType, p)
}

// PruneColumns implements LogicalPlan interface.
func (p *LogicalJoin) PruneColumns(parentUsedCols []*expression.Column) error {
	leftCols, rightCols := p.extractUsedCols(parentUsedCols)

	err := p.children[0].PruneColumns(leftCols)
	if err != nil {
		return err
	}
	addConstOneForEmptyProjection(p.children[0])

	err = p.children[1].PruneColumns(rightCols)
	if err != nil {
		return err
	}
	addConstOneForEmptyProjection(p.children[1])

	p.mergeSchema()
	if p.JoinType == LeftOuterSemiJoin || p.JoinType == AntiLeftOuterSemiJoin {
		joinCol := p.schema.Columns[len(p.schema.Columns)-1]
		parentUsedCols = append(parentUsedCols, joinCol)
	}
	p.inlineProjection(parentUsedCols)
	return nil
}

// PruneColumns implements LogicalPlan interface.
func (la *LogicalApply) PruneColumns(parentUsedCols []*expression.Column) error {
	leftCols, rightCols := la.extractUsedCols(parentUsedCols)

	err := la.children[1].PruneColumns(rightCols)
	if err != nil {
		return err
	}
	addConstOneForEmptyProjection(la.children[1])

	la.CorCols = extractCorColumnsBySchema4LogicalPlan(la.children[1], la.children[0].Schema())
	for _, col := range la.CorCols {
		leftCols = append(leftCols, &col.Column)
	}

	err = la.children[0].PruneColumns(leftCols)
	if err != nil {
		return err
	}
	addConstOneForEmptyProjection(la.children[0])

	la.mergeSchema()
	return nil
}

// PruneColumns implements LogicalPlan interface.
func (p *LogicalLock) PruneColumns(parentUsedCols []*expression.Column) error {
	if !IsSelectForUpdateLockType(p.Lock.LockType) {
		return p.baseLogicalPlan.PruneColumns(parentUsedCols)
	}

	if len(p.partitionedTable) > 0 {
		// If the children include partitioned tables, there is an extra partition ID column.
		parentUsedCols = append(parentUsedCols, p.extraPIDInfo.Columns...)
	}

	for _, cols := range p.tblID2Handle {
		for _, col := range cols {
			for i := 0; i < col.NumCols(); i++ {
				parentUsedCols = append(parentUsedCols, col.GetCol(i))
			}
		}
	}
	return p.children[0].PruneColumns(parentUsedCols)
}

// PruneColumns implements LogicalPlan interface.
func (p *LogicalWindow) PruneColumns(parentUsedCols []*expression.Column) error {
	windowColumns := p.GetWindowResultColumns()
	cnt := 0
	for _, col := range parentUsedCols {
		used := false
		for _, windowColumn := range windowColumns {
			if windowColumn.Equal(nil, col) {
				used = true
				break
			}
		}
		if !used {
			parentUsedCols[cnt] = col
			cnt++
		}
	}
	parentUsedCols = parentUsedCols[:cnt]
	parentUsedCols = p.extractUsedCols(parentUsedCols)
	err := p.children[0].PruneColumns(parentUsedCols)
	if err != nil {
		return err
	}

	p.SetSchema(p.children[0].Schema().Clone())
	p.Schema().Append(windowColumns...)
	return nil
}

func (p *LogicalWindow) extractUsedCols(parentUsedCols []*expression.Column) []*expression.Column {
	for _, desc := range p.WindowFuncDescs {
		for _, arg := range desc.Args {
			parentUsedCols = append(parentUsedCols, expression.ExtractColumns(arg)...)
		}
	}
	for _, by := range p.PartitionBy {
		parentUsedCols = append(parentUsedCols, by.Col)
	}
	for _, by := range p.OrderBy {
		parentUsedCols = append(parentUsedCols, by.Col)
	}
	return parentUsedCols
}

// PruneColumns implements LogicalPlan interface.
func (p *LogicalLimit) PruneColumns(parentUsedCols []*expression.Column) error {
	if len(parentUsedCols) == 0 { // happens when LIMIT appears in UPDATE.
		return nil
	}

	savedUsedCols := make([]*expression.Column, len(parentUsedCols))
	copy(savedUsedCols, parentUsedCols)
	if err := p.children[0].PruneColumns(parentUsedCols); err != nil {
		return err
	}
	p.schema = nil
	p.inlineProjection(savedUsedCols)
	return nil
}

func (*columnPruner) name() string {
	return "column_prune"
}

// By add const one, we can avoid empty Projection is eliminated.
// Because in some cases, Projectoin cannot be eliminated even its output is empty.
func addConstOneForEmptyProjection(p LogicalPlan) {
	proj, ok := p.(*LogicalProjection)
	if !ok {
		return
	}
	if proj.Schema().Len() != 0 {
		return
	}

	constOne := expression.NewOne()
	proj.schema.Append(&expression.Column{
		UniqueID: proj.ctx.GetSessionVars().AllocPlanColumnID(),
		RetType:  constOne.GetType(),
	})
	proj.Exprs = append(proj.Exprs, &expression.Constant{
		Value:   constOne.Value,
		RetType: constOne.GetType(),
	})
}
