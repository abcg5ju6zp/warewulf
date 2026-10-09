# Profile 事务化更新与派生结果协调重建 - 产品需求文档

## Overview

- **Summary**: 将 profile 更新（API `PATCH /api/profiles/{id}`、CLI `wwctl profile set/unset`）从"直接改对象 → 截断写 nodes.conf → reload"的松散流程，改造为"计算完整依赖闭包与受影响节点 → 循环与引用可用性校验 → 原子发布 nodes.conf → daemon adopt 新版本 → 按节点×system/runtime 独立、可重试地重建 overlay"的事务化流程，并用持久化 journal 保留 publish / adopt / overlays 三个恢复阶段，使任意失败（含进程重启）后都能从断点继续。
- **Purpose**: 解决被嵌套引用的基础 profile 变更后，镜像、内核参数、overlay 等已展开到节点上的派生结果不在同一次变更中重新确认，写盘或部分 overlay 构建失败后不同节点继续使用不同代继承结果的问题。
- **Target Users**: 通过 REST API 或 `wwctl` 修改 profile 的集群管理员；负责排障与重试的运维人员；依赖重启后自愈能力的无人值守环境。

## Goals

- 更新生效前完成依赖闭包、受影响节点、循环引用、镜像/overlay 可用性的完整校验，不合法变更不落盘。
- nodes.conf 以原子方式发布；失败时保留上一完整版本可读。
- daemon 采用新版本后，仅对真正受影响的节点分别重建 system 与 runtime overlay；单个节点/单个 context 失败彼此隔离。
- publish、adopt（daemon reload）、overlays（每节点×每 context）三类状态持久化，失败可重试，进程重启后继续未完成工作。
- 保持既有行为兼容：未受影响节点、显式节点覆盖、否定 profile（`~name`）、纯查询、`autobuild overlays: false`、手动/lazy overlay 构建、profile/node 的 add/delete 入口。

## Non-Goals

- 不改造 node add/update/delete、profile add/delete 的业务语义（它们仅因 PersistToFile 改造而获得原子写）。
- 不新增对外 REST 端点；PATCH 成功响应体仍为更新后的 profile JSON。
- 不修改 overlay 模板渲染、`wwctl overlay build`、节点 PXE 请求触发 lazy autobuild 的现有逻辑。
- 不处理多 warewulfd 实例间的协调（假定单一活动 server 与单一 nodes.conf/journal 文件）。
- 不变更镜像导入、内核版本选择机制本身。

## Background & Context

现状关键代码路径：

- API 更新：[profile.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/warewulfd/api/profile.go#L109-L158) `updateProfile()` —— 仅校验**输入对象本身**的直接 profile/image/overlay 引用，mergo 合并后 `registry.Persist()` + `warewulfd.Reload()`。
- CLI 更新：[main.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/app/wwctl/profile/set/main.go#L174-L178) 与 [unset/main.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/app/wwctl/profile/unset/main.go#L90-L96) —— `Persist()` + `warewulfd.DaemonReload()`（`/usr/sbin/service warewulfd reload` → SIGHUP）。
- 写盘：[modifiers.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/node/modifiers.go#L116-L141) `PersistToFile()` 使用 `O_TRUNC` 直接覆盖，写到一半崩溃会留下截断/半文件。
- daemon reload：[nodedb.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/warewulfd/nodedb.go#L129-L137) 进程内重读 nodes.conf；SIGHUP 与启动入口见 [server.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/warewulfd/server/server.go#L81-L92)。
- profile 展开：[mergo.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/node/mergo.go#L27-L67) 用 `visited` map **静默容忍循环**；`~name` 为否定 token，[methods.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/node/methods.go#L393-L418) `cleanList` 在合并后移除被否定项。
- 反向查询仅有单层：[queries.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/node/queries.go#L26-L50) `ListNodesUsingProfile` / `ListProfilesUsingProfile` 不处理嵌套。
- overlay 构建：[overlay.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/overlay/overlay.go#L759-L803) `BuildAllOverlays`（多 worker，任一构建错误经 errChan 返回首个错误）、[overlay.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/overlay/overlay.go#L907-L959) `BuildOverlay(node, allNodes, context, names)`；lazy 触发见 [util.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/warewulfd/util.go#L47-L77)（镜像缺失或 nodes.conf 更新则重建，受 `autobuild overlays` 开关控制，默认 true）。
- 镜像/overlay 可用性判定：`image.ValidSource()`、`overlay.Get()`（返回 `ErrDoesNotExist`）。
- 配置路径：`BuildConfig.WWProvisiondir`（默认 `$LOCALSTATEDIR/warewulf/provision`，testenv 已配置），journal 将置于其下。

已确认的决策：

1. overlay 重建为 daemon 内**后台异步**执行，请求不等待全部构建完成。
2. 配置已生效但部分节点 overlay 构建失败时，PATCH 仍返回 200，失败状态落 journal 并记录日志，后续重试。
3. 覆盖 API 与 `wwctl profile set`、`wwctl profile unset` 两个入口，共享同一事务机制。
4. journal 文件位于 `WWPROVISIONDIR/profile-change.json`。

## Functional Requirements

- **FR-1（反向依赖闭包）**: 对每个被修改的 profile（root），在**变更后**的 profile 图上计算反向传递闭包：直接或嵌套（含多跳）引用 root 的全部 profile，加 root 自身；否定边（token 以 `~` 开头）不是引用边；结果去重并按确定性顺序排序。
- **FR-2（循环检测）**: profile 引用图不允许因本次更新**引入**循环（自环也算循环）。检测在写盘之前进行；当变更前无环、变更后有环时拒绝整个更新（API 返回 400 `INVALID_ARGUMENT`，CLI 返回非零错误），错误信息包含构成循环的 profile ID。变更前已存在的环不阻止 nodes.conf 读取与 GET 查询，也不阻止与环无关或用于修复环的写入（不在本次"引入"之列）。
- **FR-3（引用可用性校验）**: 写盘前校验**本次变更新引入**的引用（存量悬空引用不阻断对无关字段的编辑，例如镜像尚未导入时修改 comment）：
  - 被修改 profile 相对变更前**新增**的非否定 profile 引用必须存在；
  - 新设置/变更后的非空 `image name` 必须通过 `image.ValidSource`；
  - 相对变更前**新增**的 system/runtime overlay 列表项必须能通过 `overlay.Get`；
  - 对每个候选受影响节点，以同样的"变更前合并结果 vs 变更后合并结果"差异方式校验新出现的 image/overlay（覆盖经由嵌套 profile 新继承到节点的引用）。
  任一不满足则拒绝更新，nodes.conf 与 journal 均不发生变化（校验失败不产生残留记录）。
- **FR-4（受影响节点精确计算）**: 候选节点 = 在变更后配置上展开嵌套 profile（沿用现有 `getNodeProfiles` 语义，含否定处理与去重）后，profile 列表与 FR-1 闭包相交的节点；最终受影响节点 = 候选中**合并后节点（`MergeNode` 完整结果）相对变更前发生变化**的节点。节点显式覆盖了继承字段、或因否定 profile 导致合并结果不变的节点，不得纳入；未受影响节点既不构建也不触碰其 overlay 镜像。
- **FR-5（原子发布）**: nodes.conf 通过"同目录临时文件写入 → fsync → `rename(2)` 覆盖 → 目录 fsync"发布；发布失败时原 nodes.conf 内容必须保持不变且仍可解析。所有现有 `Persist()` 调用方透明获得原子语义。
- **FR-6（journal 持久化）**: 在 `$WWPROVISIONDIR/profile-change.json` 维护 JSON journal（自身也以临时文件+rename 原子写，权限 0600），至少记录：变更 ID、被修改的 root profile 列表、时间戳、当前阶段（`publish`/`adopt`/`overlays`/`done`）、待发布 nodes.conf 全文与 sha256、AutoBuild 快照、每个受影响节点的 system/runtime 两条构建状态（`pending`/`done`/`failed` + 错误信息 + 尝试次数 + 更新时间）。同一进程内多个更新经互斥串行提交。
- **FR-7（阶段机与失败隔离）**: 阶段推进顺序为 publish → adopt → overlays → done。失败停留在当前阶段并记录错误，不得回滚或丢失已完成阶段；overlays 阶段每个"节点 × context（system/runtime）"是独立状态项，一项失败不阻断其他项，其他节点继续构建。
- **FR-8（daemon 后台构建 worker）**: daemon 内以单一后台 worker 串行处理 journal：adopt 完成后逐项构建；每项调用现有 `overlay.BuildOverlay(node, allNodes, "system"|"runtime", names)`；构建前若目标镜像已存在且新于 nodes.conf mtime，可直接标记 done（使管理员手动 `wwctl overlay build` 成为有效恢复手段）；全部项完成后记录置 done 并从 journal 清除，journal 为空时删除文件。每个阶段与每次成功/失败/重试写 wwlog。
- **FR-9（触发与恢复入口）**:
  - API PATCH：校验 → journal(publish) → 原子发布 → 进程内 `Reload()`（adopt）→ journal(overlays) → 唤醒 worker → 立即返回 200 与更新后 profile；
  - CLI set/unset：用户确认后、写盘前完成 FR-1~FR-4 校验；校验失败不写盘；通过后 journal+原子发布，再 `DaemonReload()`（SIGHUP 由 daemon 接管 adopt+构建）；`DaemonReload()` 失败时记录保留在 adopt 阶段并向用户报错/告警；
  - daemon：`RunServer` 初始 Reload 后、每次收到 SIGHUP Reload 后，恢复 journal（adopt 未完成则完成 adopt，继续 overlays）；
  - nodaemon 模式（测试/无 daemon）：CLI 提交后在本进程内执行一次 journal 恢复，保证同步可观察。
- **FR-10（重启续跑）**: 任意阶段进程终止后，新进程（daemon 启动/SIGHUP/后续更新/nodaemon 恢复）能仅凭 journal 续跑：publish 阶段先比对磁盘 nodes.conf 与 payload 的 sha256，已一致则跳过写入；节点已被删除则其构建项标记完成跳过；其余 pending/failed 项继续构建。
- **FR-11（autobuild 关闭兼容）**: 变更时快照 `autobuild overlays`；为 false 时不产生任何 overlay 构建项，adopt 完成后记录直接 done；不改变 lazy/manual 构建在该配置下的现有行为。
- **FR-12（兼容性边界）**:
  - GET 类端点（profiles/nodes/overlays 查询）不产生 journal、不触发构建；
  - profile add/delete、node add/update/delete 端点行为与响应不变（写盘自动获得原子性）；
  - 节点 PXE 的 lazy autobuild 路径行为不变；
  - PATCH 成功响应体 schema 不变（profile JSON，HTTP 200）；校验错误仍为 400；
  - journal 仅含本次变更涉及的受影响节点。
- **FR-13（CLI roots 与校验时机）**: `wwctl profile set/unset` 以"实际产生字段 diff 的 profile 集合"为 roots；校验（FR-1~FR-4）在确认之后、任何写盘之前执行；无实际 diff 时维持现有"No changes"退出路径。

## Non-Functional Requirements

- **NFR-1（可测试性）**: 闭包/循环/受影响节点计算、原子写、阶段机与恢复均须有单元/集成测试；阶段失败通过包内可替换的内部 hook（原子写失败、reload 失败、构建失败）注入，不依赖 root 权限或文件权限假设。
- **NFR-2（确定性与性能）**: 闭包、节点列表、journal 项顺序确定（排序后处理），便于测试与日志比对；闭包与合并比对规模为 O(P+N) 量级；后台 worker 串行构建，确保同一时刻只有一个代际在发布，避免并发构建互相覆盖。
- **NFR-3（安全）**: journal 文件权限 0600（nodes.conf 中可能含 IPMI 口令等敏感字段，journal 持有其全量副本）；目录沿用现有 provision 目录权限。
- **NFR-4（向后兼容）**: `go build ./...`、既有测试集（`go test ./...`）在不修改既有测试断言的前提下通过；确需新增测试时不得削弱既有覆盖。
- **NFR-5（可观测）**: 每次更新的 roots、受影响节点数、阶段迁移、每项构建成功/失败/重试均有 wwlog 记录；失败错误信息保留底层原因。

## Constraints

- **Technical**: Go（模块 `github.com/warewulf/warewulf`）；node 包不得反向依赖 overlay 包（现状 overlay 依赖 node），因此编排/引用校验逻辑放在 `internal/pkg/warewulfd` 包内；API 与 wwctl 均已导入该包。配置路径只能复用 `BuildConfig` 现有字段（journal 用 `WWProvisiondir`），不得改构建模板/Makefile。
- **Business**: 不新增 REST 端点、不改变既有响应 schema 与错误码大类。
- **Dependencies**: 复用现有 `node.NodesYaml`、`image.ValidSource`、`overlay.Get`/`BuildOverlay`、`warewulfd.Reload`/`DaemonReload`、testenv 测试设施。

## Assumptions

- 单一活动 warewulfd 写 nodes.conf；wwctl 与 API 的并发写由既有"每次操作重新 `node.New()` 读盘"模式与本方案的进程内提交互斥共同约束（跨进程并发写不在本次范围内，但原子 rename 保证不会出现半文件）。
- 后台 worker 内构建失败属于可重试的瞬态/配置态错误（如模板错误修复后下一次恢复即可成功）；不设置重试次数上限，attempts 仅作观测。
- journal 中保留全量新 nodes.conf 作为 publish 阶段 write-ahead 载荷；nodes.conf 体量在可接受范围。
- 合并后节点的**任意**字段变化都可能影响 overlay 模板渲染结果（模板可引用全部节点字段），故以完整合并结果深度相等作为是否重建的判据。

## Acceptance Criteria

### AC-1: 嵌套反向闭包与受影响节点计算

- **Type**: `rule`
- **Given**: 节点 n1→profiles=[a]，a→[base]，b→[a]，n2→[b]，n3 与 base 无关；修改 base。
- **When**: 执行 profile 更新事务的分析步骤。
- **Then**: 闭包为 {base,a,b}（顺序确定）；受影响节点含 n1、n2，不含 n3。
- **Pass Condition**: 新增单元测试对上述拓扑断言闭包成员与受影响节点集合完全一致。
- **Evidence**: `internal/pkg/node` 新增依赖分析测试输出。

### AC-2: 循环引用在写盘前被拒绝

- **Type**: `rule`
- **Given**: 更新会使 profile 图出现环（自环或多节点环，如 x→y、y→x）。
- **When**: 调用 API PATCH 或 CLI set/unset。
- **Then**: 返回 400 `INVALID_ARGUMENT`（API）或非零错误（CLI），错误包含环上 profile ID；nodes.conf 字节不变；无 journal 残留。
- **Pass Condition**: 环检测单元测试（自环/双节点环/三节点环/否定边不算环）+ API/CLI 集成测试断言状态码、磁盘内容与 journal 目录。
- **Evidence**: 测试命令与断言输出。

### AC-3: 引用的镜像或 overlay 不可用时拒绝更新

- **Type**: `rule`
- **Given**: 变更后 base（或受影响节点经嵌套继承的合并结果）引用不存在的 image 或 overlay。
- **When**: 发起更新。
- **Then**: 400/非零失败，nodes.conf 不变、无 journal 记录；直接引用与嵌套继承引用两种情形均覆盖。
- **Pass Condition**: 集成测试断言两类引用路径均被拦截。
- **Evidence**: 测试输出。

### AC-4: 显式节点覆盖与否定 profile 不触发无谓重建

- **Type**: `rule`
- **Given**: n1 显式设置了与 base 相同效果的字段（合并结果不因 base 修改而变）；n2 通过 `~base` 否定继承；n3 不引用闭包。
- **When**: 修改 base 并完成事务。
- **Then**: journal 受影响节点列表不含 n1/n2/n3；不触发这些节点的任何 overlay 构建，其既有镜像文件（含 mtime）不变。
- **Pass Condition**: 集成测试检查 journal 内容与未受影响镜像文件 mtime 未变化。
- **Evidence**: 测试输出。

### AC-5: nodes.conf 原子发布且失败可恢复

- **Type**: `rule`
- **Given**: 原子重命名在首次提交时被注入失败；磁盘上存在旧版 nodes.conf。
- **When**: 提交更新，随后执行恢复（模拟进程重启后调用恢复入口）。
- **Then**: 首次失败后旧 nodes.conf 仍可完整解析且内容为旧版；journal 记录处于 `publish`；恢复后 nodes.conf 为新版本且可解析，阶段继续推进。
- **Pass Condition**: 单元/集成测试读取失败前后文件内容并驱动恢复到下一阶段。
- **Evidence**: 测试输出。

### AC-6: adopt（daemon reload）失败保留独立恢复阶段

- **Type**: `rule`
- **Given**: 首次提交时注入 reload 失败。
- **When**: 提交后重启恢复（第二次移除注入）。
- **Then**: nodes.conf 已为新版但记录停留在 `adopt`；恢复后 daemon 内存 DB 采用新版（节点查询可见新值），阶段进入 overlays 并完成。
- **Pass Condition**: warewulfd 包内阶段机测试断言阶段迁移与重载后查询结果。
- **Evidence**: 测试输出。

### AC-7: 每节点×每 context 独立构建与失败隔离

- **Type**: `rule`
- **Given**: 受影响节点 n1（system 模板故障导致 system 构建失败）、n2（正常）；二者 runtime 均正常。
- **When**: 事务进入 overlays 阶段并由 worker 处理。
- **Then**: n2 的 system/runtime 与 n1 的 runtime 标记 done；n1 system 标记 failed 并记录错误；journal 保留；修复模板后再次恢复，全部 done、journal 清除。
- **Pass Condition**: 集成测试断言四项状态矩阵及修复后收敛。
- **Evidence**: 测试输出。

### AC-8: 进程重启后从未完成阶段继续

- **Type**: `rule`
- **Given**: 直接构造处于 overlays 阶段、含 pending 项的 journal（模拟进程在构建中崩溃）；overlay 镜像尚不存在。
- **When**: 以 daemon 启动方式调用恢复入口。
- **Then**: 仅 pending/failed 项被构建，已 done 项不重复构建；全部完成后 journal 被删除。
- **Pass Condition**: 测试断言构建调用/镜像文件与 journal 文件删除。
- **Evidence**: 测试输出。

### AC-9: API 成功路径端到端

- **Type**: `rule`
- **Given**: base 被 n1（经 a 嵌套）、n2 引用；n3 不引用；overlay 与 image 均存在。
- **When**: PATCH 修改 base 的 kernel args。
- **Then**: HTTP 200 且响应体为更新后的 profile JSON（schema 与现状一致）；nodes.conf 更新；n1/n2 的 system、runtime 镜像被重建为新代（mtime 晚于提交前），n3 镜像不变；journal 最终为空。
- **Pass Condition**: httptest 集成测试断言状态码、响应 JSON、磁盘 YAML、三节点镜像 mtime。
- **Evidence**: `go test ./internal/pkg/warewulfd/...` 输出。

### AC-10: autobuild 关闭时不自动构建

- **Type**: `rule`
- **Given**: warewulf.conf 设 `autobuild overlays: false`。
- **When**: 通过 API/CLI 修改被节点引用的 profile。
- **Then**: 事务在 adopt 后直接完成，journal 无 overlay 构建项且被清除；不产生新镜像文件；GET/lazy/manual 行为不变。
- **Pass Condition**: 集成测试断言无构建、无镜像、无残留 journal。
- **Evidence**: 测试输出。

### AC-11: 查询与既有入口兼容

- **Type**: `rule`
- **Given**: 现有 profile/node/overlay API 测试与 wwctl profile set/unset 测试。
- **When**: 运行既有测试集（不修改既有断言）。
- **Then**: 全部通过；GET 请求不产生 journal 文件；profile add/delete、node 增删改响应不变。
- **Pass Condition**: `go test ./...` 全绿（允许因 Go 版本/环境原因与本改动无关的既有失败，但需显式甄别记录）。
- **Evidence**: 全量测试输出与甄别记录。

### AC-12: CLI set/unset 校验前置与错误语义

- **Type**: `rule`
- **Given**: CLI set/unset 将引入环或缺失引用。
- **When**: 带 `-y` 执行命令。
- **When**: 命令非零退出，提示对应错误；nodes.conf 不变、无 journal；nodaemon 下合法变更会同步完成恢复（镜像可观察到重建）。
- **Pass Condition**: CLI 集成测试断言退出错误、磁盘不变与合法路径的重建结果。
- **Evidence**: `go test ./internal/app/wwctl/profile/...` 输出。

### AC-13: 事务编排与状态机质量

- **Type**: `rubric`
- **Dimension**: 阶段机封装的内聚性、可维护性与失败路径完备性
- **Scale**: 1-5
- **Anchors**: 1 = 阶段逻辑散落在 API/CLI/server 多处、存在未处理失败分支；3 = 主要流程集中但仍有重复或边界遗漏；5 = 单一事务编排器承载阶段定义、持久化、恢复与触发，API/CLI/server 仅做薄接入，所有注入的失败点（写盘/reload/构建）都有测试与确定行为。
- **Pass Threshold**: >= 4
- **Evidence**: 代码结构评审与失败注入测试矩阵。

### AC-14: 与现有代码风格/约定的一致性

- **Type**: `rubric`
- **Dimension**: 命名、日志、错误包装、测试风格与仓库约定的一致性
- **Scale**: 1-5
- **Anchors**: 1 = 引入新风格/绕过既有辅助函数；3 = 大体一致但有局部突兀；5 = 复用 `wwlog`、`testenv`、`status.Wrap`、现有路径与构建函数，阅读上与周边代码无分界。
- **Pass Threshold**: >= 4
- **Evidence**: 评审检查与 `go vet`/构建输出。

## Open Questions

无（四个关键设计点已与用户确认：后台异步重建；构建失败 PATCH 仍 200 并落盘重试；覆盖 API + CLI set/unset；journal 位于 WWPROVISIONDIR）。
