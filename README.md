# ontology（S1 代码骨架）

本目录实现 Spec §3（元模型：加载、编译、校验）和 §4 中 `ontoctl validate` / `export` 两个命令，附带单元测试与示例 Ontology。

## 快速开始

```bash
go mod tidy                      # 拉取依赖并生成 go.sum
make fmt                         # gofmt（源码用空格缩进，需格式化一次）
make test                        # 运行全部测试
make validate-example            # 构建并校验 examples/ontology
./bin/ontoctl validate examples/ontology --format json
./bin/ontoctl export examples/ontology
```

## 目录

```
cmd/ontoctl/            CLI（cobra）：validate、export、version
internal/spec/
  model.go              编译后的元模型类型
  typeref.go            类型字符串解析：vector(n)、array<T>、decimal(p,s)
  loader.go             递归加载 YAML，保留行列号
  compile.go            编译和全部校验规则
  constraints.go        约束校验（VT002/VT003）
  diag.go               诊断结构、排序、计数
  util.go / yamlutil.go 命名规则、snake_case、拼写建议、YAML 节点工具
  *_test.go             表驱动测试（每条规则至少一个反例）
examples/ontology/      航空维修示例（4 个对象类型、3 个链接类型）
```

## 已实现的规则

G000–G007、VT001–VT003、OT001–OT013、OT015、LT001–LT005、AT001–AT010、WH001–WH002。

规则说明：
- OT011–OT015 和 LT005 是对 Spec 的补充：非法类型、非法 sensitivity、properties 为空、columnMapping 引用了未知属性、join table 引用了未声明的 connector。
- 字段名写错（G005）会直接报错，不会被静默忽略。
- `actionTypes`、`webhooks`、`migrations` 这三种顶层 kind 已完整编译：动作规则 AT001–AT010，webhook 规则 WH001–WH002。AT003（CEL 布尔表达式校验）依赖 `internal/expr` 通过 `spec.SetExprHooks` 注入的校验器。

## 与 Spec 的差异 / 留到 S2

- 结构校验靠代码里的字段白名单，没有用嵌入的 JSON Schema。
- 暂不包括：MG 系列规则、SARIF 输出。AT/WH 规则、CEL、diff、publish、ddl 均已实现。
- `Property.Column` 目前表示**源表**中的列名，物化表的 DDL 映射在 S2 由 `objstore` 生成。

## S1-B：objstore + 本地开发环境

```bash
make dev-up               # 启动 PostgreSQL、OpenFGA、Temporal（含 UI）、mock-mro，并执行数据库迁移
make ddl-example          # 打印对象表 DDL
make test                 # 单元测试（不依赖数据库）
make test-integration     # 集成测试（需先 dev-up；会删除并重建 obj schema）
make dev-down
```

| 服务 | 本地地址 |
|---|---|
| PostgreSQL | `localhost:5432`（onto/onto，库 `ontology`） |
| OpenFGA HTTP | `localhost:8090` |
| Temporal gRPC / UI | `localhost:7233` / `localhost:8233` |
| mock MRO（MockServer） | `localhost:1080` |

`internal/objstore` 的组成：
- `ddl.go`：`GenerateDDL` 和 `PlanDDL(old, new)` 生成可重复执行的建表、加列、外键索引和多对多链接表语句。列类型变化会直接报错，需要用 S2 的 castProperty 迁移处理。向量属性不写入 PostgreSQL。
- `codec.go`：按属性类型校验并转换值。decimal 和 date 用 text 传输；JSON 类型以 `::text::jsonb` 写入，读出后解析；timestamp 统一转成 UTC。
- `store.go`：
  - 读：`Get`、`GetMany`、`Find`（eq/in 条件，按主键分页）、`Links`（外键和多对多双向遍历）。
  - 写：`ApplyEdits(tx, edits)`。
    - CREATE 遇到主键冲突返回 `ErrAlreadyExists`。
    - MODIFY 和 DELETE 带乐观锁，版本不一致返回 `ErrStale`，对象不存在返回 `ErrNotFound`。
    - LINK 和 UNLINK：外键链接改写持有外键的那一侧对象；多对多链接 upsert 或软删除链接表中的行。
    - 同一批次多次修改同一对象时，版本号自动接续。
    - 出错返回 `*EditError`，带出错编辑的序号，事务由调用方回滚。
- 物化列名是属性 apiName 的 snake_case，`spec.Property.Column` 仍然表示**源表**列名。
- S1 限制：已软删除对象的主键不能再次 CREATE；查询条件只支持 eq/in。

## S2：Action 规则 + CEL 表达式 + diff

```bash
go mod tidy && make fmt
make test                 # 包含 spec 与 expr 的单元测试
./bin/ontoctl validate examples/ontology
./bin/ontoctl export examples/ontology > /tmp/v1.json
# 改动 examples/ontology 后，和上一版对比，破坏性变更缺迁移时退出码为 1：
./bin/ontoctl diff examples/ontology --against /tmp/v1.json --format markdown
```

新增：
- **`internal/spec/action.go`**：Action 类型、参数、规则、副作用、webhook、迁移的模型定义。`CompiledOntology` 现在带 `ActionTypes` / `Webhooks` / `Migrations`。
- **`internal/spec/compile_action.go`**：AT001–AT010、WH001–WH002 校验。参数支持 `objectRef<T>`、`objectRefList<T>` 和标量；规则支持 createObject / modifyObject / deleteObject / addLink / removeLink；副作用限制至多一个 WRITEBACK webhook。
- **`internal/expr`**：基于 cel-go 的表达式环境。`submissionCriteria` 必须编译为 bool，模板里的每个 `{{...}}` 都会校验。objectRef 参数作为动态 map，支持 `now()`、`uuid()` 和成员运算符 `in`。通过 `bridge.go` 的 init 注入到 spec，避免循环依赖——`spec` 不反向依赖 `expr`。
- **`internal/spec/diff.go`**：`Diff(old, new)` 列出增删改，并标记破坏性变更；`MigrationCoverage` 找出缺少迁移声明的破坏性变更（对应 PUB002）。收紧 enum、删属性、改主键、改类型都算破坏性；加可选属性、加类型、加 Action 不算。
- **`ontoctl diff --against <json> [--format text|json|markdown] [--allow-constraint-tightening]`**：存在未声明迁移的破坏性变更时退出码为 1，可直接用于 CI。

与 Spec 的差异：
- Action 参数必须写成 `{ type: ... }` 的映射，不支持 `name: string` 简写。
- CEL 的多跳链接访问目前只在运行时解析；编译期不校验链接路径（留到 Action Service 集成时做）。
- webhook 模板的引用校验只检查括号是否配对，因为 webhook 定义本身不绑定到某一个 Action。

## S2：OMS 发布服务（发布状态机 + 元数据 API）

```bash
go mod tidy && make fmt
make test                 # 含 omsclient 缓存单测
make dev-up               # PG + 迁移
make test-integration     # 含 oms 发布状态机集成测试
make run-oms              # 本地启动 OMS（读 ONTO_DB_DSN）
```

发布（ontoctl 把源文件 POST 给 OMS，服务端重新编译校验）：
```bash
curl -s -X POST localhost:8080/api/v1/ontologies/mro/versions \
  -H 'X-Actor: me@acme' \
  -d "{\"gitCommit\":\"$(git rev-parse HEAD)\",\"files\":[{\"path\":\"ontology.yaml\",\"content\":\"...\"}]}"
```

新增：
- **`internal/oms/store.go`**：版本、类型注册（RID）、按 checksum 查重、激活等持久化操作。发布时用 `SELECT ... FOR UPDATE` 锁住 ontology 行来串行化。
- **`internal/oms/service.go`**：发布状态机（Spec §5.3）——编译校验 → checksum 查重（幂等）→ 锁 + diff + 迁移检查（PUB002）→ 分配版本和 RID + 写 PENDING → 提交 → 在独立事务里应用 DDL → 激活并发 NOTIFY。破坏性变更缺迁移返回 409 PUB002；DDL 失败把版本标为 FAILED 并返回 PUB003。回滚（activate 旧版本）前会检查 DDL 是否兼容，不兼容则拒绝。服务端始终重新编译，不信任客户端产物。
- **`internal/oms/handler.go`**：REST API——发布、版本列表/详情、激活、compiled（带 ETag 和 If-None-Match）、对象类型、Action 类型（附参数 JSON Schema）。
- **`internal/oms/listener.go`**：LISTEN/NOTIFY 监听 `oms_version` 通道，断线重连。
- **`internal/omsclient`**：读侧客户端，带 LRU 版本缓存（默认 3 个版本）和 active 指针；收到版本变更通知后下次 `Active()` 会重新拉取。`HTTPLoader` 从运行中的 OMS 拉 compiled。Action Service 后续会用它。
- **`cmd/oms`**：HTTP 服务入口，优雅关停。
- **`internal/platform/apperr`、`httpx`**：统一错误类型（带 code 和 HTTP status）和 JSON 读写辅助。

RID 规则：同一 `(ontology, kind, apiName)` 跨版本复用同一个 RID；集成测试验证了加属性后 RID 不变。

与 Spec 的差异：
- `dropProperty` 迁移在 MVP 里只做软删除（保留物理列），DDL 本身只做加列和建表。
- 认证（JWT）尚未接入，发布者身份暂时从 `X-Actor` 头取;正式环境要换成 OIDC。

## S3：Action Service（执行引擎 + 提交 + 幂等 + 乐观锁）

```bash
go mod tidy && make fmt
make test                          # 含 internal/action 引擎单测（不依赖数据库）
make dev-up && make test-integration   # 含真实 PG 的提交、审计、并发幂等测试
make run-oms                       # 终端 1：:8080
make run-action                    # 终端 2：:8081（开发模式，授权放行）
curl -s -X POST localhost:8081/api/v1/ontologies/mro/actions/groundAircraft/apply \
  -H 'X-Actor: alice' -H 'Idempotency-Key: k1' \
  -d '{"parameters":{"aircraft":"B-1234","reason":"bird strike"}}'
```

执行流程（`internal/action/engine.go`，对应 Spec §7.3）：
1. 解析 ontology 版本和动作类型
2. 幂等检查：同一个 key、相同参数时直接返回原结果；参数不同返回 409 `IDEMPOTENCY_KEY_REUSED`
3. 参数解析：一次收集全部问题（未知参数、缺必填、类型或约束不符），填默认值
4. 加载引用对象：不存在和无权查看都返回 `OBJECT_NOT_FOUND`，避免泄露对象是否存在
5. 动作权限：Agent 本身和它代表的用户都要通过；授权服务不可用时返回 503，失败关闭
6. 逐条执行前置条件（CEL），返回每条的结果
7. 仅校验模式到这里结束，不写任何数据
8. 内置规则展开为编辑，或调用注册的 Go 函数（函数只能通过受权限约束的只读视图读数据，上限 10 万个对象）
9. 编辑后校验：属性存在、类型和约束、必填属性，单次最多编辑 1 万个对象
10. 一个事务内写入：`action_run` + 对象编辑（带乐观锁）+ `object_edit`（含编辑前快照）+ outbox 事件 + 待执行的副作用记录
11. 版本冲突时从第 4 步重读重试一次，仍冲突返回 409 `STALE_OBJECT`；并发请求用同一个 key 时，靠数据库唯一索引保证只提交一次，其余请求返回赢家的结果

接口（`handler.go`）：`/validate`、`/apply`、`/applyBatch`（每条独立事务，最多 100 条）、`GET /actionRuns/{rid}`（只返回给发起人，含副作用状态）。

新增文件：
- `internal/action/`：`engine.go`、`params.go`、`values.go`、`rules.go`、`function.go`、`pgcommit.go`、`handler.go`、`types.go`
- `internal/action/functions/ground_aircraft.go`：示例 Go 函数（停场 + 提升该机未关闭工单优先级 + 新建检查工单），示例 YAML 已改为 `runtime: go`
- `internal/authz/`：OpenFGA HTTP Check 客户端，以及仅供开发用的 `AllowAll`
- `cmd/action-service/`：服务入口；监听 `oms_version` 通知刷新本地缓存

**本迭代的限制（下一步 Temporal 迭代解决）：**
- 带 WRITEBACK webhook 的动作（例如示例里的 `assignWorkOrder`）目前直接返回 501 `WRITEBACK_NOT_SUPPORTED`，不会提交任何数据。
- 副作用（通知、SIDE_EFFECT webhook）只记录为 PENDING，还没有执行它们的 worker。
- 身份目前从 `X-Actor` 等请求头读取，仅限开发；正式环境必须换成 OIDC JWT 校验。
- TypeScript / Python 函数运行时未实现，只支持 `runtime: go`。
- 返回结果还没有做属性脱敏（Spec §8.2）。

## S3：Temporal 工作流 + Webhook 写回 + 补偿 + 副作用执行

```bash
go mod tidy && make fmt
make test                 # 含 webhook 执行器、工作流（Temporal 测试环境）和引擎单测
make dev-up               # 含 Temporal（:7233，UI :8233）和 mock-mro
migrate 000002            # dev-up 会自动执行；新增 action_run.plan 列
make run-oms && make run-action   # action-service --role=all：HTTP + Temporal worker
```

带写回的动作（例如 `assignWorkOrder`）现在的流程：
1. 引擎完成校验、生成编辑，并用当时的对象数据渲染好所有 webhook 和通知（URL、body、补偿调用）。
2. 把执行记录存为 `WRITEBACK_PENDING`，执行计划写进 `action_run.plan`。此时对象还没有修改。
3. 启动工作流 `action-run:<rid>`，最多同步等待 `ONTO_ACTION_SYNC_WAIT`（默认 10s）；超时返回 `WRITEBACK_PENDING`，之后可通过 `GET /actionRuns/{rid}` 查结果。
4. 工作流按顺序执行：
   - 先调外部系统写回。按 webhook 的 retry 配置重试（429、5xx、网络错误会重试，其他 4xx 不重试）。最终失败时执行记录为 `FAILED`，接口返回 424 `WRITEBACK_FAILED`，ontology 不变。
   - 写回成功后提交编辑。提交是幂等的，基础设施错误会重试；业务冲突（版本冲突、对象已存在等）不重试，直接进入补偿。
   - 提交失败时调用补偿接口，执行记录为 `COMPENSATED`，接口返回 409 和原始错误码。没有定义补偿接口或补偿失败时，标记为 `WRITEBACK_INCONSISTENT` 并打错误日志，需要人工处理。
   - 提交成功后启动副作用工作流 `side-effects:<rid>`。它独立于主工作流运行，单个副作用失败只记录，不回滚编辑。
5. 不带写回的动作仍然同步提交，提交后再启动副作用工作流。

每次调用外部系统都会写入 `side_effect_run`，包括请求、响应（截断到 8 KB）、尝试次数和状态。请求头带稳定的 `Idempotency-Key: <rid>:<name>:<mode>`，已成功的副作用重试时不会再发。

Webhook 执行器（`internal/webhook`）：
- 主机白名单 `ONTO_WEBHOOK_ALLOWED_HOSTS`（支持 `*.internal` 这种后缀通配），并且拒绝重定向，防止通过跳转绕过白名单（SSRF）。
- 认证方式：bearer、basic、oauth2_client_credentials（token 会缓存）。密钥目前只支持 `env://NAME`，`vault://` 会明确报错。
- 审计记录里只保存密钥引用，不保存密钥本身。

部署：`--role=api`（只跑 HTTP，负责启动工作流）、`--role=worker`（只跑 Temporal worker）、`--role=all`（本地开发用）。

已知限制：
- `vault://` 密钥未接入。
- 通知只写日志（`LogNotifier`），还没有真正的邮件或消息集成。
- 工作流已启动但服务在返回前崩溃时，执行记录会停在 `WRITEBACK_PENDING`，直到工作流完成后被更新；还没有清扫任务处理 Temporal 启动失败后遗留的 PENDING 副作用。
- 写回路径还没有基于真实 PG + Temporal 的端到端集成测试，目前靠单元测试和 Temporal 测试环境覆盖。

## ontoctl publish：打通发布流程

```bash
make run-oms                      # 终端 1：OMS :8080
make publish-example              # 终端 2：发布 examples/ontology
./bin/ontoctl publish examples/ontology --oms http://localhost:8080 --dry-run   # 只看变更，不建版本
```

流程：
1. 本地先编译校验（与 `ontoctl validate` 相同）。有错误直接退出，不发请求；`--skip-local-validation` 可跳过本地这一步，服务端仍会再校验。
2. 收集目录下所有 `*.yaml` / `*.yml`，路径一律为相对路径、`/` 分隔、排序后发送（总大小上限 8 MB）。
3. `POST {oms}/api/v1/ontologies/{apiName}/versions`，OMS 重新编译、diff、检查迁移、建表、激活。
4. 输出结果：
   - 新版本：`published mro version 3 (ACTIVE)`，并列出变更
   - 内容没变：`unchanged: mro version 2 already has this content`
   - `--dry-run`：只列出会发生的变更
   - 被拒绝（校验失败、PUB002 缺迁移、PUB003 建表失败）：打印错误码和明细，退出码 1

参数与环境变量：

| 参数 | 环境变量 | 说明 |
|---|---|---|
| `--oms` | `ONTO_OMS_URL` | OMS 地址，必填 |
| `--git-commit` | `GITHUB_SHA` | 记录到版本上的提交号；`--dry-run` 时可省略 |
| — | `ONTO_TOKEN` | 作为 `Authorization: Bearer` 发送（为后续 OIDC 预留） |
| `--actor` | `ONTO_ACTOR` | 开发环境 OMS 用 `X-Actor` 识别发布者 |
| `--allow-constraint-tightening` | — | 不因收紧约束而拒绝 |
| `--format text\|json` | — | json 便于 CI 解析 |
| `--timeout` | — | 默认 2 分钟（发布要执行 DDL） |

CI 示例（GitHub Actions）：

```yaml
- run: ./bin/ontoctl validate ontology --strict
- run: ./bin/ontoctl publish ontology --oms $OMS_URL --dry-run      # PR 上只看变更
- if: github.ref == 'refs/heads/main'
  run: ./bin/ontoctl publish ontology --oms $OMS_URL                # 合并后真正发布；GITHUB_SHA 自动带上
  env: { ONTO_TOKEN: ${{ secrets.ONTO_TOKEN }} }
```

限制：OMS 目前只认 `X-Actor`，还不校验 `ONTO_TOKEN`；接入 OIDC 后才算有真正的发布权限控制。

## CI（GitHub Actions）

`.github/workflows/ci.yml` 在 push 到 main 和每个 PR 上运行两个 job：
- **unit**：gofmt 检查、`go build`、`go vet`（含 integration tag）、`go test -race`、校验示例模型并生成 DDL，覆盖率作为 artifact 上传。
- **integration**（unit 通过后）：起 PostgreSQL 16 服务容器，用 psql 按顺序执行 `migrations/*.up.sql`，再串行（`-p 1`）跑 objstore、oms、action 的集成测试——这几个包会重置同一个库的 schema，不能并行。

Temporal 工作流测试用 SDK 自带的测试环境，在 unit job 里跑，不需要 Temporal 服务。

首次提交前：本地没有 Go 时可以直接推，CI 会在缺少 `go.sum` 时自动 `go mod tidy`（并给出 warning）；但 gofmt 检查会失败，因为部分源码还没格式化。建议在有 Go 的机器上先执行 `go mod tidy && make fmt`，把 `go.sum` 一起提交。
