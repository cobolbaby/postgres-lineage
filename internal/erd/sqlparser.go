package erd

import (
	"crypto/md5"
	"encoding/json"
	"fmt"

	pg_query "github.com/pganalyze/pg_query_go/v2"
)

type Relation struct {
	Schema         string
	RelName        string
	Alias          string
	Relpersistence string
}

type Column struct {
	Schema  string
	RelName string
	Field   string
	Alias   string
}

type RelationShip struct {
	SColumn *Column
	TColumn *Column
	Type    string
}

// 解析sql，暂时不支持 PL/pgSQL
func Parse(sql string) (map[string]*RelationShip, error) {

	// Debugger
	// resultJson, err := pg_query.ParseToJSON(sql)
	// if err != nil {
	// 	panic(err)
	// }
	// fmt.Println(resultJson)

	result, err := pg_query.Parse(sql)
	if err != nil {
		return nil, err
	}

	m := make(map[string]*RelationShip)

	for _, v := range result.Stmts {
		// 判断为哪种类型SQL
		// truncate 跳过
		// drop    跳过
		// vacuum  跳过
		// analyze 跳过
		// create index 跳过
		// insert  解析其中的 select 子句
		// delete  仅解析关联删除
		// update  仅解析关联更新
		// create  解析其中的 select 子句
		// select  已经解析

		if v.Stmt.GetTruncateStmt() != nil ||
			v.Stmt.GetDropStmt() != nil ||
			v.Stmt.GetVacuumStmt() != nil ||
			v.Stmt.GetIndexStmt() != nil {
			continue
		}

		if v.Stmt.GetCreateTableAsStmt() != nil {
			r := parseSelectStmt(v.Stmt.GetCreateTableAsStmt().GetQuery().GetSelectStmt())
			m = MergeMap(m, r)
			continue
		}
		if v.Stmt.GetSelectStmt() != nil {
			r := parseSelectStmt(v.Stmt.GetSelectStmt())
			m = MergeMap(m, r)
			continue
		}
		if v.Stmt.GetInsertStmt() != nil {
			r := parseSelectStmt(v.Stmt.GetInsertStmt().GetSelectStmt().GetSelectStmt())
			m = MergeMap(m, r)
			continue
		}
		if v.Stmt.GetDeleteStmt() != nil {
			r := parseDeleteStmt(v.Stmt.GetDeleteStmt())
			m = MergeMap(m, r)
			continue
		}
		if v.Stmt.GetUpdateStmt() != nil {
			r := parseUpdateStmt(v.Stmt.GetUpdateStmt())
			m = MergeMap(m, r)
			continue
		}
	}

	return m, nil
}

func parseSelectStmt(selectStmt *pg_query.SelectStmt) map[string]*RelationShip {
	aliasMap := make(map[string]*Relation)
	m := make(map[string]*RelationShip)

	// 解析 CTE
	r0 := parseWithClause(selectStmt.GetWithClause(), aliasMap)
	m = MergeMap(m, r0)

	// 解析 FROM 获取关系
	// 从 FromClause 中获取 JoinExpr 信息，以便提炼关系
	// 从 FromClause 中获取别名信息，可能在 WHERE 会用到
	for _, vv := range selectStmt.GetFromClause() {
		r1 := parseFromClause(vv, aliasMap)
		m = MergeMap(m, r1)
	}

	// 解析 WHERE IN 获取关系
	r2 := parseWhereClause(selectStmt.GetWhereClause(), aliasMap)
	m = MergeMap(m, r2)

	return m
}

func parseDeleteStmt(deleteStmt *pg_query.DeleteStmt) map[string]*RelationShip {
	// fmt.Printf("parseDeleteStmt: %s\n", deleteStmt)
	return nil
}

func parseUpdateStmt(updateStmt *pg_query.UpdateStmt) map[string]*RelationShip {
	// fmt.Printf("parseUpdateStmt: %s\n", updateStmt)
	return nil
}

func (r *RelationShip) ToString() string {
	if r.SColumn == nil || r.TColumn == nil {
		return ""
	}

	var sDisplayName, tDisplayName string

	if r.SColumn.Schema == "" {
		sDisplayName = r.SColumn.RelName
	} else {
		sDisplayName = fmt.Sprintf("%s.%s", r.SColumn.Schema, r.SColumn.RelName)
	}
	if r.TColumn.Schema == "" {
		tDisplayName = r.TColumn.RelName
	} else {
		tDisplayName = fmt.Sprintf("%s.%s", r.TColumn.Schema, r.TColumn.RelName)
	}

	return fmt.Sprintf("%s.%s %s %s.%s",
		sDisplayName, r.SColumn.Field,
		r.Type,
		tDisplayName, r.TColumn.Field,
	)
}

func parseWithClause(withClause *pg_query.WithClause, aliasMap map[string]*Relation) map[string]*RelationShip {
	m := make(map[string]*RelationShip)

	for _, v := range withClause.GetCtes() {

		// 解析 FROM 获取关系
		// 从 FromClause 中获取 JoinExpr 信息，以便提炼关系
		// 从 FromClause 中获取别名信息，可能在 WHERE 会用到
		for _, vv := range v.GetCommonTableExpr().GetCtequery().GetSelectStmt().GetFromClause() {
			r1 := parseFromClause(vv, aliasMap)
			m = MergeMap(m, r1)
		}

		// 解析 WHERE IN 获取关系
		r2 := parseWhereClause(v.GetCommonTableExpr().GetCtequery().GetSelectStmt().GetWhereClause(), aliasMap)
		m = MergeMap(m, r2)

		// 记录 CTE 的 Alias
		r := &Relation{
			Alias:   v.GetCommonTableExpr().GetCtename(),
			RelName: v.GetCommonTableExpr().GetCtename(),
		}
		aliasMap[r.Alias] = r
	}

	return m
}

func parseFromClause(node *pg_query.Node, aliasMap map[string]*Relation) map[string]*RelationShip {
	// 包含 JOIN
	if node.GetJoinExpr() != nil {
		return parseJoinClause(node, aliasMap)
	}

	// 只是简单的一张表
	if node.GetRangeVar() != nil {
		return parseRangeVar(node.GetRangeVar(), aliasMap)
	}

	// TODO:调用 UDF，获取返回值
	// ...

	return nil
}

func parseRangeVar(node *pg_query.RangeVar, aliasMap map[string]*Relation) map[string]*RelationShip {
	lRelation := &Relation{
		RelName:        node.GetRelname(),
		Schema:         node.GetSchemaname(),
		Alias:          node.GetAlias().GetAliasname(),
		Relpersistence: node.GetRelpersistence(),
	}

	aliasMap[lRelation.Alias] = lRelation

	return nil
}

func parseWhereClause(node *pg_query.Node, aliasMap map[string]*Relation) map[string]*RelationShip {
	var relationShip map[string]*RelationShip

	if node.GetSubLink() != nil { // in
		relationShip = parseSubLink(node.GetSubLink(), pg_query.JoinType_JOIN_INNER, aliasMap)
	} else if node.GetBoolExpr() != nil {
		relationShip = parseBoolExpr(node.GetBoolExpr(), pg_query.JoinType_JOIN_INNER, aliasMap)
	}

	return relationShip
}

func parseSubLink(node *pg_query.SubLink, jointype pg_query.JoinType, aliasMap map[string]*Relation) map[string]*RelationShip {
	var relationShip map[string]*RelationShip

	switch node.GetSubLinkType() {
	case pg_query.SubLinkType_ANY_SUBLINK:
		relationShip = parseAnySubLink(node, jointype, aliasMap)
	// case :
	// 扩展...
	default:
		fmt.Printf("node.GetSubLinkType(): %s", node.GetSubLinkType())
	}

	return relationShip
}

func parseAnySubLink(node *pg_query.SubLink, jointype pg_query.JoinType, aliasMap map[string]*Relation) map[string]*RelationShip {
	// fmt.Printf("parseAnySubLink: %s", node)

	// 跳过 func(alias.name) IN (SELECT col FROM ...) 类似的SQL，变化太多，不考虑
	if node.GetTestexpr().GetFuncCall() != nil {
		return nil
	}
	// 跳过 alias.name IN (SELECT func(col) FROM ...)
	if node.GetSubselect().GetSelectStmt().GetTargetList()[0].GetResTarget().GetVal().GetFuncCall() != nil {
		return nil
	}
	// 跳过 name IN (SELECT col FROM ...)，字段不知道是哪个表的
	if len(node.GetTestexpr().GetColumnRef().GetFields()) < 2 {
		return nil
	}

	relationship := &RelationShip{}

	lrel := aliasMap[node.GetTestexpr().GetColumnRef().GetFields()[0].GetString_().GetStr()]
	relationship.SColumn = &Column{
		Schema:  lrel.Schema,
		RelName: lrel.RelName,
		Field:   node.GetTestexpr().GetColumnRef().GetFields()[1].GetString_().GetStr(),
	}

	rrel := node.GetSubselect().GetSelectStmt().GetFromClause()[0].GetRangeVar()
	relationship.TColumn = &Column{
		Schema:  rrel.GetSchemaname(),
		RelName: rrel.GetRelname(),
		Field:   node.GetSubselect().GetSelectStmt().GetTargetList()[0].GetResTarget().GetVal().GetColumnRef().GetFields()[0].GetString_().GetStr(),
	}

	relationship.Type = jointype.String()

	// checksum
	m := make(map[string]*RelationShip)
	key := Hash(relationship)
	m[key] = relationship

	return m
}

// 返回左右表间的关系，所以主体有两个表，外加关系，多张表的话，则需要返回子结果集
func parseJoinClause(node *pg_query.Node, aliasMap map[string]*Relation) map[string]*RelationShip {
	if node.GetJoinExpr() == nil {
		return nil
	}

	m := make(map[string]*RelationShip)
	j := node.GetJoinExpr()

	// 优先遍历内层JOIN
	if j.GetLarg().GetJoinExpr() != nil {
		lSubRelationShip := parseJoinClause(j.GetLarg(), aliasMap)
		m = MergeMap(m, lSubRelationShip)
	}
	if j.GetRarg().GetJoinExpr() != nil {
		rSubRelationShip := parseJoinClause(j.GetRarg(), aliasMap)
		m = MergeMap(m, rSubRelationShip)
	}
	// 处理子查询
	if j.GetLarg().GetRangeSubselect() != nil {
		// 解析 FROM
		for _, vv := range j.GetLarg().GetRangeSubselect().GetSubquery().GetSelectStmt().GetFromClause() {
			r1 := parseFromClause(vv, aliasMap)
			m = MergeMap(m, r1)
		}

		// 解析 WHERE IN 获取关系
		r2 := parseWhereClause(j.GetLarg().GetRangeSubselect().GetSubquery().GetSelectStmt().GetWhereClause(), aliasMap)
		m = MergeMap(m, r2)

		// 记录 SubQuery 的 Alias
		r := &Relation{
			Alias:   j.GetLarg().GetRangeSubselect().GetAlias().GetAliasname(),
			RelName: j.GetLarg().GetRangeSubselect().GetAlias().GetAliasname(),
		}
		aliasMap[r.Alias] = r
	}
	if j.GetRarg().GetRangeSubselect() != nil {
		// 解析 FROM
		for _, vv := range j.GetRarg().GetRangeSubselect().GetSubquery().GetSelectStmt().GetFromClause() {
			r1 := parseFromClause(vv, aliasMap)
			m = MergeMap(m, r1)
		}

		// 解析 WHERE IN 获取关系
		r2 := parseWhereClause(j.GetRarg().GetRangeSubselect().GetSubquery().GetSelectStmt().GetWhereClause(), aliasMap)
		m = MergeMap(m, r2)

		// 记录 SubQuery 的 Alias
		r := &Relation{
			Alias:   j.GetRarg().GetRangeSubselect().GetAlias().GetAliasname(),
			RelName: j.GetRarg().GetRangeSubselect().GetAlias().GetAliasname(),
		}
		aliasMap[r.Alias] = r
	}
	if j.GetLarg().GetRangeVar() != nil {
		parseRangeVar(j.GetLarg().GetRangeVar(), aliasMap)
	}
	if j.GetRarg().GetRangeVar() != nil {
		parseRangeVar(j.GetRarg().GetRangeVar(), aliasMap)
	}
	// 记录关系
	currRelationShip := parseQuals(j, aliasMap)
	m = MergeMap(m, currRelationShip)

	return m
}

func parseQuals(node *pg_query.JoinExpr, aliasMap map[string]*Relation) map[string]*RelationShip {
	if node.GetQuals() == nil {
		return nil
	}

	var relationShip map[string]*RelationShip

	if node.GetQuals().GetAExpr() != nil {
		relationShip = parseAExpr(node.GetQuals().GetAExpr(), node.GetJointype(), aliasMap)
	} else if node.GetQuals().GetBoolExpr() != nil {
		relationShip = parseBoolExpr(node.GetQuals().GetBoolExpr(), node.GetJointype(), aliasMap)
	}
	// ...

	return relationShip
}

func parseBoolExpr(expr *pg_query.BoolExpr, joinType pg_query.JoinType, aliasMap map[string]*Relation) map[string]*RelationShip {
	m := make(map[string]*RelationShip)
	for _, v := range expr.GetArgs() {
		if v.GetAExpr() != nil {
			m = MergeMap(m, parseAExpr(v.GetAExpr(), joinType, aliasMap))
		} else if v.GetSubLink() != nil { // in
			m = MergeMap(m, parseSubLink(v.GetSubLink(), joinType, aliasMap))
		} else if v.GetBoolExpr() != nil {
			m = MergeMap(m, parseBoolExpr(v.GetBoolExpr(), joinType, aliasMap))
		}
	}
	return m
}

func parseAExpr(expr *pg_query.A_Expr, joinType pg_query.JoinType, aliasMap map[string]*Relation) map[string]*RelationShip {

	l := expr.GetLexpr()
	r := expr.GetRexpr()

	if r.GetAConst() != nil { // col = 'v1'，直接跳过
		return nil
	}
	if r.GetList() != nil { // col IN ('v1', 'v2', ...)
		return nil
	}
	if r.GetAExpr() != nil { // col = 'v1' || 'v2'，那直接跳过
		return nil
	}
	if r.GetFuncCall() != nil { // col = func(...)
		return nil
	}

	relationship := &RelationShip{}

	if len(l.GetColumnRef().GetFields()) == 2 {
		rel, ok := aliasMap[l.GetColumnRef().GetFields()[0].GetString_().GetStr()]
		if !ok {
			fmt.Printf("Relation not found: %s\n", l.GetColumnRef().GetFields()[0].GetString_().GetStr())
			return nil
		}

		relationship.SColumn = &Column{
			Schema:  rel.Schema,
			RelName: rel.RelName,
			Field:   l.GetColumnRef().GetFields()[1].GetString_().GetStr(),
		}
	}

	if len(r.GetColumnRef().GetFields()) == 2 {
		rel, ok := aliasMap[r.GetColumnRef().GetFields()[0].GetString_().GetStr()]
		if !ok {
			fmt.Printf("Relation not found: %s\n", r.GetColumnRef().GetFields()[0].GetString_().GetStr())
			return nil
		}

		relationship.TColumn = &Column{
			Schema:  rel.Schema,
			RelName: rel.RelName,
			Field:   r.GetColumnRef().GetFields()[1].GetString_().GetStr(),
		}
	}

	relationship.Type = joinType.String()

	// checksum
	m := make(map[string]*RelationShip)
	key := Hash(relationship)
	m[key] = relationship

	return m
}

func Hash(s *RelationShip) string {
	data, _ := json.Marshal(s)
	return fmt.Sprintf("%x", md5.Sum(data))
}

func MergeMap(mObj ...map[string]*RelationShip) map[string]*RelationShip {
	newObj := make(map[string]*RelationShip)
	for _, m := range mObj {
		for k, v := range m {
			newObj[k] = v
		}
	}
	return newObj
}