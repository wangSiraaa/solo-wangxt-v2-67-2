# protocompat — Protobuf 发布兼容性登记服务

纯后端的 Protobuf 注册与兼容性判定服务。新增字段不等于兼容：发布入口必须给出
**有依据** 的判断。判定基于 protocompile 产出的完整链接描述符闭包（不是源码正则），
同时检查二进制线路编码与 JSON 映射两个维度；**无法证明安全的部分一律标为
`NEEDS_REVIEW`，绝不笼统标成通过**。

## 架构

```
cmd/server        ConnectRPC 服务入口（纯后端，无管理页面）
cmd/compatcheck   命令行：单树回归用例、跨包依赖锁/影响场景、两棵 proto 树临时比对
internal/schema   protocompile 封装：编译 → 描述符集 → 内容哈希 → 重新加载
internal/deps     跨包依赖锁：import 解析、固定到已登记版本、锁摘要、环检测、按锁编译
internal/impact   反向依赖图传递影响：直接/传递/经验证不受影响、原因路径、输入摘要
internal/compat   兼容性判定核心（字段号复用 / 保留删除 / 类型变化 / 枚举默认值 / 样例载荷）
internal/registry ConnectRPC 处理器、Store 接口、PostgreSQL 实现、内存实现
internal/regress  文件型回归用例运行器（CLI 与 go test 共用；含跨包场景）
testdata/cases    单树回归案例：嵌套导入、oneof 迁移、同名不同包……
testdata/impact   跨包场景：菱形依赖、传递定位、无关升级、依赖环、缺失/摘要不符、历史复现
api/registry/v1   服务契约（ConnectRPC，当前手写 handler + JSON codec，无需 codegen）
```

- **解析**：`github.com/bufbuild/protocompile`，嵌套导入、菱形导入、well-known
  types 均由编译器解析；判断在 `protoreflect` 描述符上进行。
- **跨包依赖锁**：登记版本时先解析 descriptor 中的 `import`，每个外部依赖固定到
  已登记的不可变包版本（可在 `pins` 中显式指定版本与摘要，否则固定为最新登记
  版本），并严格按这些版本的描述符闭包编译。版本落库时保存 `(package,
  version, digest)` 锁边与锁摘要 `lock_digest`。依赖缺失、环或摘要不匹配一律
  拒绝且不留任何脏数据。
- **传递影响分析**：`AnalyzeImpact` 比较某包新旧版本（本包 finding），再沿版本
  锁定的反向依赖图传播，输出 `DIRECT_IMPACTED`（直接锁到旧版且引用了变更面）、
  `TRANSITIVE_IMPACTED`（只经由其它受影响版本到达）与 `VERIFIED_UNAFFECTED`
  （锁定旧版但引用面经证据核对未受影响）三类节点，每个节点一条记录、附全部不同
  原因路径（菱形依赖保留多条路径）。结果以"输入快照摘要"做内容寻址落
  PostgreSQL：同一输入永远返回同一结果；历史分析只依赖旧版锁定的子图，最新依赖
  更新后重算仍可复现。
- **传输**：ConnectRPC（connect 协议 + JSON codec），六个方法：
  `RegisterVersion` / `CheckCompatibility` / `DeclareConsumer` / `ListVersions`
  / `AnalyzeImpact` / `GetDependencies`。
- **存储**：PostgreSQL 存包、不可变版本（描述符集 + 内容哈希 + 锁摘要）、
  路径归属、依赖边、兼容性报告、使用方声明、内容寻址的影响分析结果。
  `STORE=memory` 可本地冒烟（不持久化）。

## 判定模型

每条 finding 带 `severity`（FAIL/WARN/INFO）、`dimension`（WIRE/JSON/BOTH）、
`message`（全限定名）与 `path`（字段路径）。整体结论：

| verdict | 含义 |
|---|---|
| `COMPATIBLE` | 所有检查都有证据通过 |
| `NEEDS_REVIEW` | 没有证伪，但至少一项**无法证明**安全（如移入 oneof、取消保留号段） |
| `INCOMPATIBLE` | 至少一项被证明破坏（如字段号复用、JSON 表示变化） |

覆盖的检查（详见 `internal/compat`）：

- **字段号复用**：同号不同名且类型不同 → FAIL；同号不同名同类型 → 无法区分改名与
  换义，wire 维度 WARN、JSON 名变化 FAIL。
- **保留字段删除**：删字段且号+名都保留 → INFO；只保留号 → JSON 维度 WARN；
  什么都不保留 → WIRE 维度 FAIL。取消保留号段 / 复用保留号 → WARN。
- **类型变化**：按 wire 类型分类矩阵（varint/fixed32/fixed64/length-delimited）
  与 JSON 表示分类（number/quoted-number/bool/string/base64/enum-name/object）
  分别定级；packed repeated ↔ singular、map 键值类型、消息类型换绑等均覆盖。
- **枚举默认值**：零值（隐式默认值）改名/删除、proto2 封闭枚举新增值
  （老读者落入 unknown fields）、proto3 开放枚举新增值（INFO）。
- **oneof 迁移**：移入 → WARN（旧载荷可能同时设置互斥字段，JSON 双成员文档解析失败）；
  移出 → INFO；oneof 删除 → WARN。合成 oneof（proto3 optional）不计。
- **样例载荷**：提交者可附 `samples`（json 文本或 base64 wire），服务用新旧描述符
  分别解析并比对确定性 wire 字节与 protojson 输出，差异定位到
  `acme.Msg.lines[2].amount` 这样的字段路径；无法验证的样例记 WARN，不算通过。

## 使用方声明

消费方声明自己读取的消息/字段与编码（wire/json/both），`CheckCompatibility`
带上 `consumer` 后，报告会投影到该消费方的实际使用面：wire-only 消费者不受
纯 JSON 破坏影响，反之亦然。

## 运行

```bash
docker compose up --build        # PostgreSQL + 服务（:8080）
# 或本地：
STORE=memory go run ./cmd/server # 冒烟用，不持久化
```

注册版本（发布入口，自动与上一版本比对）：

```bash
curl -s localhost:8080/registry.v1.Registry/RegisterVersion -H 'Content-Type: application/json' -d '{
  "package": "acme.pay",
  "version": "v2",
  "files": [{"path": "pay/invoice.proto", "content": "syntax = \"proto3\"; ..."}],
  "samples": [{"message": "acme.pay.Invoice", "encoding": "json", "data": "{\"id\":\"i-1\",\"total\":5}"}],
  "require_compatible": true
}'
```

- 同版本同内容 → 幂等成功（`already_existed: true`）。
- **同版本不同内容 → `already_exists` 错误，拒绝覆盖**。
- `require_compatible: true` 且结论为 `INCOMPATIBLE` → `failed_precondition`，
  版本不落库。

声明消费方并做投影检查：

```bash
curl -s localhost:8080/registry.v1.Registry/DeclareConsumer -H 'Content-Type: application/json' -d '{
  "package": "acme.pay", "consumer": "ledger", "encoding": "wire",
  "usages": [{"message": "acme.pay.Invoice", "fields": ["id"]}]
}'
curl -s localhost:8080/registry.v1.Registry/CheckCompatibility -H 'Content-Type: application/json' -d '{
  "package": "acme.pay", "base_version": "v1", "candidate_version": "v2", "consumer": "ledger"
}'
```

## 跨包依赖锁与传递影响

被依赖包必须**先登记**；登记上游时，服务从 descriptor 中解析 `import`，按文件
路径归属找到所属包，并把每个依赖固定到不可变版本（默认最新，可用 `pins`
显式固定版本并校验摘要）：

```bash
# 1) 先登记基础包
curl -s localhost:8080/registry.v1.Registry/RegisterVersion -H 'Content-Type: application/json' -d '{
  "package": "acme.common", "version": "v1",
  "files": [{"path": "base/common.proto", "content": "syntax = \"proto3\"; package acme.common; message Money { string currency = 1; }"}]
}'
# 2) 登记 billing（import base/common.proto），响应回显锁定的 (package,version,digest)
curl -s localhost:8080/registry.v1.Registry/RegisterVersion -H 'Content-Type: application/json' -d '{
  "package": "acme.billing", "version": "v1",
  "files": [{"path": "middle/invoice.proto", "content": "syntax = \"proto3\"; package acme.billing; import \"base/common.proto\"; message Line { acme.common.Money amount = 1; }"}]
}'
# 显式固定版本 + 锁摘要校验（lockfile 式）：摘要不符直接 failed_precondition
#   "pins": [{"package": "acme.common", "version": "v1", "digest": "<sha256>"}]

# 3) 查询某版本当时锁定的依赖摘要
curl -s localhost:8080/registry.v1.Registry/GetDependencies -H 'Content-Type: application/json' -d '{
  "package": "acme.billing", "version": "v1"
}'

# 4) 登记不兼容的 common v2（currency: string -> int32）
curl -s localhost:8080/registry.v1.Registry/RegisterVersion -H 'Content-Type: application/json' -d '{
  "package": "acme.common", "version": "v2",
  "files": [{"path": "base/common.proto", "content": "syntax = \"proto3\"; package acme.common; message Money { int32 currency = 1; }"}]
}'

# 5) 传递影响分析：本包 finding + 反向依赖图，每个节点只出现一次，原因路径全保留
curl -s localhost:8080/registry.v1.Registry/AnalyzeImpact -H 'Content-Type: application/json' -d '{
  "package": "acme.common", "base_version": "v1", "candidate_version": "v2"
}'
```

拒绝规则（均为 `failed_precondition`，登记事务整体回滚，不留版本/路径/依赖边）：

- import 的路径没有任何已登记包提供（依赖缺失）；
- 显式 pin 的版本不存在，或 pin 携带的摘要与登记内容不一致（摘要不匹配）；
- 新登记会闭合包级依赖环（如 A 锁 B、B 又锁 A）。

分析结果中节点状态：`DIRECT_IMPACTED` / `TRANSITIVE_IMPACTED` /
`VERIFIED_UNAFFECTED`；`reason_paths` 给出每条原因链与边上的证据符号。菱形
依赖（同一目标经两条链到达）只产生一条目标记录但保留多条路径；未锁定旧版的
包（例如只锁了新版）不会出现，引用面未变更的锁定方会被显式标记为
`VERIFIED_UNAFFECTED` 而不是沉默。分析按输入快照内容寻址：重复请求返回同一
`input_digest` 与同一结果，历史版本始终使用当时锁定的依赖，即使依赖后来发布了
新版本。

## 命令行回归

```bash
go run ./cmd/compatcheck run testdata/cases      # 全部单树回归案例
go run ./cmd/compatcheck impact testdata/impact   # 跨包依赖锁/传递影响场景
go run ./cmd/compatcheck check -old A -new B -format text   # 临时比对两棵 proto 树
go test ./...                                    # 单元测试 + 同一套回归案例
```

`testdata/cases` 下每个案例是 `old/`、`new/` 两棵 proto 树加 `case.json` 期望，
覆盖嵌套导入（菱形依赖）、oneof 迁移、同名不同包、字段号复用、保留/未保留删除、
枚举默认值变化、类型变化矩阵与样例载荷。期望未列出的 WARN/FAIL 会使案例失败——
沉默不算通过。

`testdata/impact` 下每个场景是 `scenario.json` 加各包版本的 proto 目录，通过
内存版登记服务真实驱动 RegisterVersion/AnalyzeImpact，覆盖：

| 场景 | 验收点 |
|---|---|
| `diamond_paths` | 菱形依赖只产生一条目标记录，但保留两条原因路径；引用未变更类型的锁定方为 `VERIFIED_UNAFFECTED` |
| `transitive_break` | 依赖包不兼容变更沿 l1→l2→top 传递定位到顶层，直接/传递分类正确 |
| `unrelated_upgrade` | 只引用未变更类型的锁定方经验证不受影响；不锁该包的包完全不出现（不误报） |
| `cycle_reject` | 依赖环登记被原子拒绝，版本/路径/边均不留脏数据，既有锁摘要不变 |
| `missing_dependency` | import 无对应已登记包时拒绝，无版本落库 |
| `digest_mismatch` | pin 摘要与登记内容不一致时拒绝，无版本落库 |
| `historical_lock` | 历史版本始终使用当时锁定依赖；新依赖发布后重算，输入摘要与结果完全一致；v1→v3 只命中锁定 v1 的消费者 |

## 测试

```bash
go test ./...                    # 全部（PostgreSQL 集成测试在无 DATABASE_URL 时跳过）
DATABASE_URL='postgres://postgres@localhost:5433/registry?sslmode=disable&host=/tmp' \
  go test ./internal/registry/   # 含锁存储、原子环拒绝、影响结果落库的端到端 PG 用例
```
