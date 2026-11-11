package db

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// 每个测试包独占一个 PostgreSQL schema。
//
// `go test ./...` 会把所有包指向同一个 ARTEX_PG_DSN：共用 public schema 时，先跑的包
// 留下的行（llm_profiles 的唯一名字、settings 单例、tasks、intercept_pending…）会让后跑
// 的包失败，表现为「单包全绿、整跑飘红」。agent 包此前用一把 advisory lock 把套件串起来，
// 只挡住了并发，挡不住残留。
//
// 这里在唯一的连接入口（Open / DSN）上做隔离：测试二进制（os.Args[0] 形如 <pkg>.test）
// 首次取 DSN 时重建 t_<pkg> schema 并把 search_path 指过去，各包互不可见、每次运行都是
// 干净的初始状态（等价于 CI 里新建库）。生产进程（二进制名不是 *.test）行为不变。

var (
	testSchemaOnce   sync.Once
	testSchemaName   string
	testSchemaReady  bool
	testSchemaSource string

	isolatedDSNs sync.Map // 原始 DSN → 已注入 search_path 的 DSN（或原值）
)

// testSchemaNameFor 返回当前测试二进制的独占 schema 名；非测试进程返回空串。
func testSchemaNameFor() string {
	testSchemaOnce.Do(func() {
		base := filepath.Base(os.Args[0])
		if !strings.HasSuffix(base, ".test") {
			return
		}
		var b strings.Builder
		for _, r := range strings.ToLower(strings.TrimSuffix(base, ".test")) {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
				b.WriteRune(r)
			default:
				b.WriteByte('_')
			}
		}
		if b.Len() == 0 {
			return
		}
		testSchemaName = "t_" + b.String()
	})
	return testSchemaName
}

// resetTestSchema 重建本包的 schema（每个进程一次）。schema 名由包名派生、只含
// [a-z0-9_]，内联进 DDL 是安全的。失败则退回原 DSN，让测试按原有语义跳过/报错。
func resetTestSchema(dsn, schema string) bool {
	if testSchemaSource == dsn {
		return testSchemaReady
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		return false
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		return false
	}
	if _, err := admin.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`); err != nil {
		return false
	}
	if _, err := admin.Exec(`CREATE SCHEMA "` + schema + `"`); err != nil {
		return false
	}
	testSchemaSource, testSchemaReady = dsn, true
	return true
}

// isolateTestSchema 给测试连接注入 search_path。任何一步失败都退回原 DSN：
// 隔离是测试便利设施，不该改变「连不上就跳过/报错」的既有语义。
func isolateTestSchema(dsn string) string {
	schema := testSchemaNameFor()
	if schema == "" || dsn == "" {
		return dsn
	}
	if cached, ok := isolatedDSNs.Load(dsn); ok {
		return cached.(string)
	}
	isolated := dsn
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		if resetTestSchema(dsn, schema) {
			q := u.Query()
			q.Set("search_path", schema) // pgx 把未知参数当运行时参数，落到启动包
			u.RawQuery = q.Encode()
			isolated = u.String()
		}
	}
	isolatedDSNs.Store(dsn, isolated)
	return isolated
}
