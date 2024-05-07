package lineage

import (
	"strings"
	"time"

	"pg_lineage/pkg/depgraph"
	"pg_lineage/pkg/log"

	pg_query "github.com/pganalyze/pg_query_go/v5"
	"github.com/tidwall/gjson"
)

var (
	PLPGSQL_BLACKLIST_STMTS = map[string]bool{
		"PLpgSQL_stmt_assign":     true,
		"PLpgSQL_stmt_raise":      true,
		"PLpgSQL_stmt_execsql":    false,
		"PLpgSQL_stmt_if":         true,
		"PLpgSQL_stmt_dynexecute": true, // 比较复杂，不太好支持
		"PLpgSQL_stmt_perform":    true, // 暂不支持
	}
)

const (
	REL_PERSIST     = "p"
	REL_PERSIST_NOT = "t"
)

type Owner struct {
	Username string
	Nickname string
	ID       string
}

type Record struct {
	SchemaName string
	RelName    string
	Type       string
	Columns    []string
	Comment    string
	Visited    string
	Size       int64
	Layer      string
	Database   string
	Owner      *Owner
	CreateTime time.Time
	Labels     []string
	ID         string
}

func (r *Record) GetID() string {
	if r.ID != "" {
		return r.ID
	}

	if r.SchemaName != "" {
		return r.SchemaName + "." + r.RelName
	} else {
		switch r.RelName {
		case "pg_namespace", "pg_class", "pg_attribute", "pg_type":
			r.SchemaName = "pg_catalog"
			return r.SchemaName + "." + r.RelName
		default:
			return r.RelName
		}
	}
}

func (r *Record) IsTemp() bool {
	return r.SchemaName == "" ||
		strings.HasPrefix(r.RelName, "temp_") ||
		strings.HasPrefix(r.RelName, "tmp_")
}

type Op struct {
	Type       string
	ProcName   string
	SchemaName string
	Database   string
	Comment    string
	Owner      *Owner
	SrcID      string
	DestID     string
	ID         string
}

func (o *Op) GetID() string {
	if o.ID != "" {
		return o.ID
	}

	if o.SchemaName == "" {
		o.SchemaName = "public"
	}
	return o.SchemaName + "." + o.ProcName
}

func ParseUDF(plpgsql string) (*depgraph.Graph, error) {

	sqlTree := depgraph.New()

	raw, err := pg_query.ParsePlPgSqlToJSON(plpgsql)
	if err != nil {
		return nil, err
	}
	// log.Debugf("pg_query.ParsePlPgSqlToJSON: %s", raw)

	v := gjson.Parse(raw).Array()[0]

	for _, action := range v.Get("PLpgSQL_function.action.PLpgSQL_stmt_block.body").Array() {
		action.ForEach(func(key, value gjson.Result) bool {
			// 没有配置，或者屏蔽掉的
			if enable, ok := PLPGSQL_BLACKLIST_STMTS[key.String()]; ok && enable {
				return false
			}

			// 递归调用 Parse
			if err := parseUDFOperator(sqlTree, key.String(), value.String()); err != nil {
				log.Errorf("pg_query.ParseToJSON err: %s, sql: %s", err, value.String())
				return false
			}

			return true
		})
	}

	return sqlTree, nil
}

func parseUDFOperator(sqlTree *depgraph.Graph, operator, plan string) error {
	// log.Printf("%s: %s\n", operator, plan)

	var subQuery string

	switch operator {
	case "PLpgSQL_stmt_execsql":
		subQuery = gjson.Get(plan, "sqlstmt.PLpgSQL_expr.query").String()

		// 跳过不必要的SQL，没啥解析的价值
		if subQuery == "select clock_timestamp()" {
			return nil
		}

	case "PLpgSQL_stmt_dynexecute":
		// 支持 execute dynamic sql
		subQuery = gjson.Get(plan, "query.PLpgSQL_expr.query").String()

	}

	if err := parseSQL(sqlTree, subQuery); err != nil {
		return err
	}

	return nil
}

func Parse(sql string) (*depgraph.Graph, error) {
	sqlTree := depgraph.New()

	if err := parseSQL(sqlTree, sql); err != nil {
		return nil, err
	}

	return sqlTree, nil
}

func parseSQL(sqlTree *depgraph.Graph, sql string) error {

	log.Debugf("%s\n", sql)
	result, err := pg_query.Parse(sql)
	if err != nil {
		return err
	}

	for _, s := range result.Stmts {

		// 跳过 drop/truncate/create index/analyze/vacuum/set 语句
		if s.Stmt.GetTruncateStmt() != nil ||
			s.Stmt.GetDropStmt() != nil ||
			s.Stmt.GetVacuumStmt() != nil ||
			s.Stmt.GetIndexStmt() != nil ||
			s.Stmt.GetVariableSetStmt() != nil {
			break
		}

		// create table ... as
		if s.Stmt.GetCreateTableAsStmt() != nil {
			ctas := s.Stmt.GetCreateTableAsStmt()

			tnode := parseRangeVar(ctas.GetInto().GetRel())
			sqlTree.AddNode(tnode)

			if ctas.GetQuery().GetSelectStmt() != nil {

				// with ... select ...
				// select ... union select ...
				// select ... from ...

				ss := ctas.GetQuery().GetSelectStmt()

				if ss.GetWithClause() != nil {
					parseWithClause(ss.GetWithClause(), sqlTree)
				}

				for _, r := range parseSelectStmt(ss) {
					sqlTree.DependOn(tnode, r)
				}

			}
		}

		// create table ...
		if s.Stmt.GetCreateStmt() != nil {
			cs := s.Stmt.GetCreateStmt()

			tnode := parseRangeVar(cs.GetRelation())
			sqlTree.AddNode(tnode)
		}

		// insert into ...
		if s.Stmt.GetInsertStmt() != nil {
			is := s.Stmt.GetInsertStmt()

			tnode := parseRangeVar(is.GetRelation())
			sqlTree.AddNode(tnode)

			// // with ... select * from ...
			// if is.GetWithClause() != nil {
			// 	parseWithClause(is.GetWithClause(), sqlTree)
			// }

			// select * from ...
			if is.GetSelectStmt() != nil {

				ss := is.GetSelectStmt()

				// with ... select * from ...
				if ss.GetWithClause() != nil {
					parseWithClause(ss.GetWithClause(), sqlTree)
				}

				for _, r := range parseSelectStmt(ss.GetSelectStmt()) {
					sqlTree.DependOn(tnode, r)
				}
			}
		}

		// delete from ...
		// delete from ... using ... where ...
		if s.Stmt.GetDeleteStmt() != nil {
			ds := s.Stmt.GetDeleteStmt()

			tnode := parseRangeVar(ds.GetRelation())
			sqlTree.AddNode(tnode)

			// 关联删除，依赖 using 关键词
			if ds.GetUsingClause() != nil {
				for _, r := range parseUsingClause(ds.GetUsingClause()) {
					sqlTree.DependOn(tnode, r)
				}
			}

			// 关联删除，依赖 where 关键词
			// if ds.GetWhereClause() != nil {
			// 	for _, r := range parseWhereClause(ds.GetWhereClause()) {
			// 		sqlTree.DependOn(tnode, r)
			// 	}
			// }
		}

		// update ... set ...
		// update ... set ... from ...
		// update ... set ... from (select * from tbl2) tbl3 where ...
		if s.Stmt.GetUpdateStmt() != nil {
			us := s.Stmt.GetUpdateStmt()

			tnode := parseRangeVar(us.GetRelation())
			sqlTree.AddNode(tnode)

			if us.GetFromClause() != nil {
				for _, r := range parseUsingClause(us.GetFromClause()) {
					sqlTree.DependOn(tnode, r)
				}
			}
		}

		// select ... from ...
		if s.Stmt.GetSelectStmt() != nil {
			ss := s.Stmt.GetSelectStmt()
			for _, r := range parseSelectStmt(ss) {
				sqlTree.AddNode(r)
			}
		}

	}

	return nil
}

// INSERT / UPDATE / DELETE / CREATE TABLE 单表操作
func parseRangeVar(node *pg_query.RangeVar) *Record {

	// var alias string

	// if node.GetAlias().GetAliasname() != "" {
	// 	alias = node.GetAlias().GetAliasname()
	// } else {
	// 	alias = node.GetRelname()
	// }

	return &Record{
		RelName:    node.GetRelname(),
		SchemaName: node.GetSchemaname(),
		Type:       node.GetRelpersistence(), // relpersistence 不同，全局临时表为 s ，普通临时表为 t
	}

}

// CTE 子句
func parseWithClause(wc *pg_query.WithClause, sqlTree *depgraph.Graph) error {

	for _, cte := range wc.GetCtes() {
		tnode := &Record{
			RelName:    cte.GetCommonTableExpr().GetCtename(),
			SchemaName: "",
			Type:       REL_PERSIST_NOT,
		}
		sqlTree.AddNode(tnode)

		// 如果存在 FROM 字句，则需要添加依赖关系
		for _, r := range parseSelectStmt(cte.GetCommonTableExpr().GetCtequery().GetSelectStmt()) {
			sqlTree.DependOn(tnode, r)
		}
	}

	return nil
}

// FROM Clause
func parseSelectStmt(ss *pg_query.SelectStmt) []*Record {
	var records []*Record

	// 如果遇到 UNION，则调用 parseUnionClause 方法
	if ss.GetOp() == pg_query.SetOperation_SETOP_UNION {
		if r := parseUnionClause(ss); r != nil {
			records = append(records, r...)
		}
	}

	for _, fc := range ss.GetFromClause() {

		// 最简单的 select 查询，只有一个表
		if fc.GetRangeVar() != nil {
			records = append(records, parseRangeVar(fc.GetRangeVar()))
		}
		// 子查询
		if fc.GetRangeSubselect() != nil {
			if r := parseSelectStmt(fc.GetRangeSubselect().GetSubquery().GetSelectStmt()); r != nil {
				records = append(records, r...)
			}
		}
		// 关联查询
		if fc.GetJoinExpr() != nil {
			if r := parseJoinClause(fc.GetJoinExpr()); r != nil {
				records = append(records, r...)
			}
		}
	}

	return records
}

// UNION 解析
func parseUnionClause(ss *pg_query.SelectStmt) []*Record {
	var records []*Record

	if ss.GetOp() != pg_query.SetOperation_SETOP_UNION {
		return records
	}

	if r := parseSelectStmt(ss.GetLarg()); r != nil {
		records = append(records, r...)
	}
	if r := parseSelectStmt(ss.GetRarg()); r != nil {
		records = append(records, r...)
	}
	return records
}

// JOIN Clause
func parseJoinClause(jc *pg_query.JoinExpr) []*Record {
	var records []*Record

	larg := jc.GetLarg()
	rarg := jc.GetRarg()

	if larg.GetRangeVar() != nil {
		records = append(records, parseRangeVar(larg.GetRangeVar()))
	}
	if rarg.GetRangeVar() != nil {
		records = append(records, parseRangeVar(rarg.GetRangeVar()))
	}
	if larg.GetRangeSubselect() != nil {
		if r := parseSelectStmt(larg.GetRangeSubselect().GetSubquery().GetSelectStmt()); r != nil {
			records = append(records, r...)
		}
	}
	if rarg.GetRangeSubselect() != nil {
		if r := parseSelectStmt(rarg.GetRangeSubselect().GetSubquery().GetSelectStmt()); r != nil {
			records = append(records, r...)
		}
	}
	if larg.GetJoinExpr() != nil {
		if r := parseJoinClause(larg.GetJoinExpr()); r != nil {
			records = append(records, r...)
		}
	}
	if rarg.GetJoinExpr() != nil {
		if r := parseJoinClause(rarg.GetJoinExpr()); r != nil {
			records = append(records, r...)
		}
	}

	return records
}

// 关联删除，关联更新
func parseUsingClause(uc []*pg_query.Node) []*Record {
	var records []*Record

	for _, r := range uc {
		// 只关联了一张表
		if r.GetRangeVar() != nil {
			records = append(records, parseRangeVar(r.GetRangeVar()))
		}
	}

	return records
}

func parseWhereClause(wc *pg_query.Node) []*Record {
	var records []*Record

	if wc.GetSubLink() != nil {
		log.Debugf("parseWhereClause: %v", wc.GetSubLink())
	}

	return records
}
