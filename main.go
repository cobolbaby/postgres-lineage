package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/cobolbaby/lineage/internal/erd"
	"github.com/cobolbaby/lineage/internal/lineage"
	"github.com/cobolbaby/lineage/pkg/depgraph"
	"github.com/cobolbaby/lineage/pkg/log"
	_ "github.com/lib/pq"
	"github.com/neo4j/neo4j-go-driver/v4/neo4j"
)

var (
	PLPGSQL_UNHANLED_COMMANDS   = regexp.MustCompile(`(?i)set\s+(time zone|enable_)(.*?);`)
	PLPGSQL_GET_FUNC_DEFINITION = `
		SELECT nspname, proname, pg_get_functiondef(p.oid) as definition
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE nspname = '%s' and proname = '%s'
		LIMIT 1;
	`
	PG_QUERY_STORE = `
		SELECT 
			s.query, s.calls, s.total_time, s.min_time, s.max_time, s.mean_time
		FROM 
			pg_stat_statements s
		JOIN
			pg_database d ON d.oid = s.dbid
		WHERE
			d.datname = '%s'
			AND calls > 10
		ORDER BY
			s.mean_time DESC
		Limit 1000;
	`
	PG_FuncCallPattern1 = regexp.MustCompile(`(?i)(select|call)\s+(\w+)\.(\w+)\((.*)\)\s*(;)?`)
	PG_FuncCallPattern2 = regexp.MustCompile(`(?i)select\s+(.*)from\s+(\w+)\.(\w+)\((.*)\)\s*(as\s+(.*))?\s*(;)?`)
)

type DataSource struct {
	Alias string
	Name  string
	DB    *sql.DB
}

type QueryStore struct {
	Query     string
	Calls     uint32
	TotalTime float64
	MinTime   float64
	MaxTime   float64
	MeanTime  float64
}

func init() {
	if err := log.InitLogger(&log.LoggerConfig{
		Level: "info",
		Path:  "./logs/lineage.log",
	}); err != nil {
		fmt.Println(err)
	}
}

func main() {
	// db, err := sql.Open("postgres", "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable")
	db, err := sql.Open("postgres", DB_DSN)
	if err != nil {
		log.Fatal("sql.Open err: ", err)
	}
	defer db.Close()

	uri, _ := url.Parse(DB_DSN)
	ds := &DataSource{
		Alias: DB_ALIAS,
		Name:  strings.TrimPrefix(uri.Path, "/"),
		DB:    db,
	}

	driver, err := neo4j.NewDriver(NEO4J_URL, neo4j.BasicAuth(NEO4J_USER, NEO4J_PASSWORD, ""))
	if err != nil {
		log.Fatal("neo4j.NewDriver err: ", err)
	}
	// Handle driver lifetime based on your application lifetime requirements  driver's lifetime is usually
	// bound by the application lifetime, which usually implies one driver instance per application
	defer driver.Close()

	// ???????????????????????????????????????????????????????????????????????????
	if err := lineage.ResetGraph(driver); err != nil {
		log.Fatal("ResetGraph err: ", err)
	}
	if err := erd.ResetGraph(driver); err != nil {
		log.Fatal("ResetGraph err: ", err)
	}

	// ????????????pg_stat_statements??????sql??????
	querys, err := ds.DB.Query(fmt.Sprintf(PG_QUERY_STORE, ds.Name))
	if err != nil {
		log.Fatal("db.Query err: ", err)
	}
	defer querys.Close()

	m := make(map[string]*erd.RelationShip)

	for querys.Next() {

		var qs QueryStore
		_ = querys.Scan(&qs.Query, &qs.Calls, &qs.TotalTime, &qs.MinTime, &qs.MaxTime, &qs.MeanTime)

		// ??????????????????????????????????????????????????????udf?????????????????????????????????????????????????????????
		// generateTableLineage(&qs, ds, driver)

		// ?????????????????????????????????????????? MAP ???????????????????????????????????????????????????
		m = erd.MergeMap(m, generateTableJoinRelation(&qs, ds, driver))

		// ???????????????.
	}

	// ???????????????...
	if err := erd.CreateGraph(driver, m); err != nil {
		log.Errorf("ERD err: %s ", err)
	}

}

// ???????????? JOIN ???
// ????????????????????????????????? IN / JOIN
func generateTableJoinRelation(qs *QueryStore, ds *DataSource, driver neo4j.Driver) map[string]*erd.RelationShip {
	log.Debugf("generateTableJoinRelation sql: %s", qs.Query)

	var m map[string]*erd.RelationShip

	if udf, err := IdentifyFuncCall(qs.Query); err == nil {
		m, _ = HandleUDF4ERD(ds.DB, udf)
	} else {
		m, _ = erd.Parse(qs.Query)
	}

	n := make(map[string]*erd.RelationShip)
	for kk, vv := range m {
		// ??????????????????
		if vv.SColumn == nil || vv.TColumn == nil || vv.SColumn.Schema == "" || vv.TColumn.Schema == "" {
			continue
		}
		n[kk] = vv
	}
	fmt.Printf("GetRelationShip: #%d\n", len(n))
	for _, v := range n {
		fmt.Printf("%s\n", v.ToString())
	}

	return n
}

// ????????????????????????
func generateTableLineage(qs *QueryStore, ds *DataSource, driver neo4j.Driver) {

	// ?????? UDF ?????????
	udf, err := IdentifyFuncCall(qs.Query)
	if err != nil {
		return
	}
	// udf = &Op{
	// 	Type:       "plpgsql",
	// 	ProcName:   "func_insert_fact_sn_info_f6",
	// 	SchemaName: "dw",
	// }
	sqlTree, err := HandleUDF4Lineage(ds.DB, udf)
	if err != nil {
		log.Errorf("HandleUDF %+v, err: %s", udf, err)
		return
	}

	log.Debugf("UDF Graph: %+v", sqlTree)
	for i, layer := range sqlTree.TopoSortedLayers() {
		log.Debugf("UDF Graph %d: %s\n", i, strings.Join(layer, ", "))
	}

	// ?????????????????????????????????????????????
	sqlTree.SetNamespace(ds.Alias)
	// ??????????????????

	if err := lineage.CreateGraph(driver, sqlTree.ShrinkGraph(), udf); err != nil {
		log.Errorf("UDF CreateGraph err: %s ", err)
	}
}

func IdentifyFuncCall(sql string) (*lineage.Op, error) {

	// ??????????????????????????????
	// select dw.func_insert_xxxx(a,b)
	// select * from report.query_xxxx(1,2,3)
	// call dw.func_insert_xxxx(a,b)

	if r := PG_FuncCallPattern1.FindStringSubmatch(sql); r != nil {
		log.Debug("FuncCallPattern1:", r[1], r[2], r[3])
		return &lineage.Op{
			Type:       "plpgsql",
			SchemaName: r[2],
			ProcName:   r[3],
		}, nil
	}
	if r := PG_FuncCallPattern2.FindStringSubmatch(sql); r != nil {
		log.Debug("FuncCallPattern2:", r[1], r[2], r[3])
		return &lineage.Op{
			Type:       "plpgsql",
			SchemaName: r[2],
			ProcName:   r[3],
		}, nil
	}

	return nil, errors.New("not a function call")
}

// ??????????????????
func HandleUDF4Lineage(db *sql.DB, udf *lineage.Op) (*depgraph.Graph, error) {
	log.Infof("HandleUDF: %s.%s", udf.SchemaName, udf.ProcName)

	// ??????????????????????????? e.g. select now()
	if udf.SchemaName == "" || udf.SchemaName == "pg_catalog" {
		return nil, fmt.Errorf("UDF %s is system function", udf.ProcName)
	}

	definition, err := GetUDFDefinition(db, udf)
	if err != nil {
		log.Errorf("GetUDFDefinition err: %s", err)
		return nil, err
	}

	// ???????????????????????? pg_query ?????? set ??????????????????
	// https://github.com/pganalyze/libpg_query/issues/125
	plpgsql := filterUnhandledCommands(definition)
	// log.Debug("plpgsql: ", plpgsql)

	sqlTree, err := lineage.ParseUDF(plpgsql)
	if err != nil {
		log.Errorf("ParseUDF %+v, err: %s", udf, err)
		return nil, err
	}

	return sqlTree, nil
}

func HandleUDF4ERD(db *sql.DB, udf *lineage.Op) (map[string]*erd.RelationShip, error) {
	log.Infof("HandleUDF: %s.%s", udf.SchemaName, udf.ProcName)

	// ??????????????????????????? e.g. select now()
	if udf.SchemaName == "" || udf.SchemaName == "pg_catalog" {
		return nil, fmt.Errorf("UDF %s is system function", udf.ProcName)
	}

	definition, err := GetUDFDefinition(db, udf)
	if err != nil {
		log.Errorf("GetUDFDefinition err: %s", err)
		return nil, err
	}

	// ???????????????????????? pg_query ?????? set ??????????????????
	// https://github.com/pganalyze/libpg_query/issues/125
	plpgsql := filterUnhandledCommands(definition)
	// log.Debug("plpgsql: ", plpgsql)

	relationShips, err := erd.ParseUDF(plpgsql)
	if err != nil {
		log.Errorf("ParseUDF %+v, err: %s", udf, err)
		return nil, err
	}

	return relationShips, nil
}

// ?????????????????????
func filterUnhandledCommands(content string) string {
	return PLPGSQL_UNHANLED_COMMANDS.ReplaceAllString(content, "")
}

// ??????????????????
func GetUDFDefinition(db *sql.DB, udf *lineage.Op) (string, error) {

	rows, err := db.Query(fmt.Sprintf(PLPGSQL_GET_FUNC_DEFINITION, udf.SchemaName, udf.ProcName))
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var nspname string
	var proname string
	var definition string

	for rows.Next() {
		err := rows.Scan(&nspname, &proname, &definition)
		switch err {
		case sql.ErrNoRows:
			log.Warn("No rows were returned")
		case nil:
			log.Infof("Query Data = (%s, %s)\n", nspname, proname)
		default:
			log.Fatalf("rows.Scan err: %s", err)
		}
	}

	return definition, nil
}
