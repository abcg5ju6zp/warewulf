# Profile 事务化更新与派生结果协调重建 - 实现计划

## Task 1: node 包 — profile 图循环检测、反向闭包与受影响节点分析

- **Status**: `completed`
- **Completion Evidence**:
  - 新增 [dependency.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/node/dependency.go)：`ProfileCycleError`、`CheckProfileCycles()`（三态 DFS，否定/悬空边忽略，自环检出）、`ProfileReverseClosure()`（反向传递闭包，排序去重）、`AffectedNodes(prev, roots)`（闭包候选 + MergeNode 全量 DeepEqual 精确化）；[constructors.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/node/constructors.go) 增加 `NodesYaml.Clone()`。
  - TR-1.1/1.2/1.3：[dependency_test.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/node/dependency_test.go) 全部通过（容器 golang:1.25）：闭包 {a,b,base}、受影响 {n1,n2}；自环/双/三节点环返回含环 ID 的 `ProfileCycleError`；否定边与悬空引用不误报；标量覆盖（image）排除、`~base` 嵌套否定排除、tags/kernel/image/overlay 变更包含；Clone 独立性。
  - 命令：`go test ./internal/pkg/node/` → ok 0.116s（整包无回归）。
  - TR-1.4 rubric 5/5：分析逻辑仅 ~120 行，直接复用 `getNodeProfiles`/`MergeNode`/`ListAllProfiles` 现有原语，无重复展开实现、无副作用。
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 新增 `internal/pkg/node/dependency.go`（及 `dependency_test.go`），实现纯数据结构层面的图分析，不依赖 overlay/image 包：
    - `CheckProfileCycles() error`：对完整 profile 图做 DFS 三态着色（white/gray/black），边为 `Profile.Profiles` 中非 `~` 前缀且目标存在的引用；发现回边时返回含环上 ID 序列的错误（自环须检出）；`~` 否定 token 与悬空引用不作为边。
    - `ProfileReverseClosure(roots []string) []string`：构建反向邻接（忽略否定边），从 roots 出发求传递闭包并集（含 roots 自身），结果排序去重。
    - `AffectedNodes(prev *NodesYaml, roots []string) []string`：在当前（变更后）registry 上先求闭包，再以 `getNodeProfiles` 展开每个节点的有效 profile 列表求候选，最后用 `reflect.DeepEqual` 比较 prev/当前两边 `MergeNode` 的完整结果，仅返回合并结果发生变化的节点 ID（排序）。
  - 复用现有 `getProfilesProfiles`/`getNodeProfiles` 的展开与否定语义，不改变其行为。
- **Acceptance Criteria Addressed**: AC-1, AC-2, AC-4
- **Test Requirements**:
  - `rule` TR-1.1: AC-1 拓扑（n1→a→base、n2→b→a、n3 无关）断言闭包恰为 {base,a,b}、受影响节点恰为 {n1,n2}；证据：`go test ./internal/pkg/node/ -run TestProfileDependency -v` 输出。
  - `rule` TR-1.2: 自环、双节点环、三节点环均返回错误且错误串包含环上 ID；无环返回 nil；否定边不构成环；悬空引用不导致误报。证据：同测试运行输出。
  - `rule` TR-1.3: 节点显式覆盖继承字段（合并结果不变）、仅经 `~base` 否定、完全无关三类节点均不在受影响集合；修改 tags/kernel/image/overlay 等不同字段时受影响集合与合并 diff 一致。证据：测试断言。
  - `rubric` TR-1.4: 维度=图分析实现的简洁与可复用性；1-5；1=重复实现展开逻辑/行为与现有合并语义漂移，3=可用但有重复，5=复用现有展开原语且无副作用；阈值>=4；证据：代码评审。

## Task 2: node 包 — nodes.conf 原子发布

- **Status**: `completed`
- **Completion Evidence**:
  - 新增 [atomicfile.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/util/atomicfile.go) `util.AtomicWriteFile`（同目录临时文件 0600 创建→chmod→写→FileSync→rename→目录 best-effort fsync，失败清理临时文件且不动原文件）。
  - [modifiers.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/node/modifiers.go) `PersistToFile` 改为原子发布（经包内 `atomicWriteConfig` 变量，默认 `util.AtomicWriteFile`），签名不变，所有调用方透明受益；移除不再使用的 os 导入。
  - TR-2.1：`TestPersistToFileAtomic` 验证内容正确、可重新 Parse、目录无 .tmp 残留；`TestAtomicWriteFileReplacesContent` 验证覆盖写、0644 权限、无残留。
  - TR-2.2：`TestPersistToFileAtomicFailureKeepsOldVersion`（注入发布失败）旧内容字节不变且可解析，恢复后成功发布新版；`TestAtomicWriteFileFailureLeavesOriginal` 验证 util 层失败原文件不动。
  - TR-2.3：`go test ./internal/pkg/node/` ok 0.177s、`go test ./internal/pkg/util/ -run TestAtomicWriteFile` PASS，无回归。
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 修改 `internal/pkg/node/modifiers.go` 的 `PersistToFile`：先 `Dump()` 得到完整字节；在目标同目录创建临时文件（`nodes.conf.tmp-*`，0600 创建后 chmod 0644 与现状一致），写入全部字节并 `FileSync`，`os.Rename` 覆盖目标，随后打开目录 `fsync`；关闭临时文件错误照常上抛；任何失败路径清理临时文件且不触碰原文件。
  - 保持函数签名与 `Persist()` 入口不变，所有现有调用方（含 node 发现路径、API/CLI 各端点）透明获得原子语义。
- **Acceptance Criteria Addressed**: AC-5, AC-11
- **Test Requirements**:
  - `rule` TR-2.1: 正常持久化后文件内容与 `Dump()` 一致且目录下无遗留临时文件；证据：`PersistToFile` 测试输出。
  - `rule` TR-2.2: 注入/构造 rename 失败（如临时文件与目标路径间设障碍，或目标路径父级不可写情形在非 root 下验证；使用可替换的内部 rename hook 保证 CI root 环境可测）时，原 nodes.conf 内容保持旧版且可被 `node.New()` 重新解析；证据：失败前后读盘断言。
  - `rule` TR-2.3: 既有 `constructors/modifiers` 相关测试不改动即通过。证据：`go test ./internal/pkg/node/`。

## Task 3: warewulfd 包 — profile 变更 journal 存储

- **Status**: `completed`
- **Completion Evidence**:
  - 新增 [profilechange_journal.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/warewulfd/profilechange_journal.go)：阶段/状态常量、`overlayBuildState`、`affectedNodeState`、`profileChangeRecord`（roots、时间戳、phase、phase_error、全量 ConfigYAML+sha256、AutoBuild、每节点×两 context 状态）、`profileChangeStore`（互斥锁、缺失文件视为空、损坏 JSON 报错、0600 原子写、空记录即删文件、自动创建父目录）。
  - TR-3.1：`TestProfileChangeJournalLifecycle` 覆盖 add→重载一致→update→remove（文件删除）→未知 ID 报错，通过。
  - TR-3.2：`TestProfileChangeJournalWriteFailureKeepsPreviousState` 注入写失败后旧记录/字节不变，故障清除后恢复；断言文件权限 0600；`TestProfileChangeJournalCorruptFile` 损坏文件返回错误。
  - 命令：`go test ./internal/pkg/warewulfd/ -run TestProfileChangeJournal -v` → 3/3 PASS（ww-golang:1.25 镜像，含 libgpgme-dev）。
  - TR-3.3 rubric 5/5：阶段×节点×context 状态模型完备，JSON 字段稳定（version/id/attempts/error/timestamps/hash）。
- **Priority**: high
- **Depends On**: Task 2
- **Description**:
  - 新增 `internal/pkg/warewulfd/profilechange.go` 中的 journal 类型与存储层（可分文件 `profilechange_journal.go`）：
    - 类型：`overlayBuildState{Status,Error,Attempts,UpdatedAt}`、`affectedNodeState{Node,System,Runtime}`、`profileChangeRecord{ID,Roots,StartedAt,UpdatedAt,Phase,ConfigYAML,ConfigHash,AutoBuild,Nodes}`，阶段常量 `phasePublish/phaseAdopt/phaseOverlays/phaseDone`，状态常量 `stPending/stDone/stFailed`。
    - 路径：`path.Join(config.Get().Paths.WWProvisiondir, "profile-change.json")`；文件 0600；读不到文件视为空 journal；损坏 JSON 返回错误（恢复调用方记录错误日志但不致命）。
    - `loadJournal()`/`saveJournal()`：保存经临时文件+rename 原子完成；`addRecord`/`updateRecord`/`removeRecord` 在互斥锁下操作；无记录时删除文件。
    - journal 自身的 marshal 不依赖 node 类型（仅字符串/基础类型），避免循环依赖。
- **Acceptance Criteria Addressed**: AC-6, AC-7, AC-8
- **Test Requirements**:
  - `rule` TR-3.1: 新增记录→落盘→重新加载字段一致；更新/删除记录后磁盘内容同步；清空后文件被删除。证据：`go test ./internal/pkg/warewulfd/ -run TestProfileChangeJournal -v`。
  - `rule` TR-3.2: 保存过程为原子写（同目录临时文件+rename），写入失败时既有 journal 仍可完整读取；文件权限位为 0600。证据：失败注入（内部 hook）后重新 load 断言。
  - `rubric` TR-3.3: 维度=状态模型表达力；1-5；1=无法区分三阶段或节点×context 粒度，3=可区分但缺观测字段，5=阶段/错误/attempts/时间戳/hash 齐备且 JSON 稳定可 diff；阈值>=4；证据：序列化快照评审。

## Task 4: warewulfd 包 — 事务编排器：校验、阶段机、后台 worker 与恢复

- **Status**: `completed`
- **Completion Evidence**:
  - 新增 [profilechange.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/warewulfd/profilechange.go)：`ErrInvalidProfileChange`（与 `ProfileCycleError` 双 %w 串联）、`validateProfileChange`（环检→root 直接引用→受影响节点合并引用）、阶段机 begin/publish/adopt、每节点×context 构建（已存在且新于 nodes.conf 则免建，节点删除自动完结）、单 worker `runProfileChangeOnce`（TryLock 合并触发）、`kickProfileChangeWorker`、`RecoverProfileChanges`、`ApplyProfileChangeDaemon`/`ApplyProfileChangeCLI`；四个失败注入 seam（publish/reload/build/external reload）。
  - [nodedb.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/warewulfd/nodedb.go) `Reload()` 改为返回 error（旧调用点忽略返回值，兼容）。
  - TR-4.1：缺失 overlay（root）/缺失 image（嵌套继承）/循环三类拒绝，nodes.conf 不变、无 journal。
  - TR-4.2：注入发布失败→记录停留 publish、旧文件可解析；恢复后发布+构建全部完成、journal 删除。
  - TR-4.3：注入 reload 失败→停留 adopt、内存 DB 仍旧；恢复后 `GetNode(hwaddr)` 可见新 tags 并完成构建。
  - TR-4.4：故障模板制造 n1 system failed；n1 runtime / n2 system/runtime 全部 done 且镜像落盘；修复模板后再次恢复全部收敛、journal 清空。
  - TR-4.5：手工构造 phaseOverlays 崩溃记录（含已删除节点）→恢复后 pending 项构建、删除节点项完结、journal 删除。
  - 附加：`TestProfileChangeManualBuildSatisfiesRecovery` 验证手动构建（镜像新于 nodes.conf）零构建调用即收敛。
  - TR-4.6：autobuild=false 时 adopt 后记录直接移除，构建 hook 0 调用、无镜像。
  - 命令：`go test ./internal/pkg/warewulfd/` ok 1.451s；`go vet` 三个改动包零告警。
  - TR-4.7 rubric 4/5（待 Task 5-7 薄接入验证后复评 5）；TR-4.8 rubric 5/5（全程 wwlog、%w 错误包装、testenv 同构测试风格）。
- **Priority**: high
- **Depends On**: Task 1, Task 2, Task 3
- **Description**:
  - 在 `internal/pkg/warewulfd/profilechange.go` 实现单一编排器（API/CLI/server 共用的唯一入口层）：
    - `BeginProfileChange(roots []string, prev, next *node.NodesYaml) (*profileChangeRecord, error)`：调用 `next.CheckProfileCycles()`；校验 roots 自身非否定 profile 引用存在；对每个 root 与受影响节点（`next.AffectedNodes(prev, roots)`）的直接/合并结果执行 image（`image.ValidSource`，空串豁免）与 system/runtime overlay（`overlay.Get`）可用性校验；构造记录（payload=`next.Dump()` 全文 + sha256，AutoBuild 快照，节点×两 context 的 pending 项），先持久化 journal（phasePublish）再返回。校验错误定义为 `ErrInvalidProfileChange`（可 errors.Is），不落任何 journal。
    - `publishRecord(rec)`：磁盘 hash 已等于 ConfigHash 则跳过；否则将 payload 经原子临时文件+rename 写到 NodesConf()（复用 Task 2 同款原子写工具，允许直接写字节而不仅限 registry），成功后转 phaseAdopt 并保存。
    - `adoptRecord(rec)`：调用进程内 `Reload()`（经可替换 hook，默认 `Reload`），成功转 phaseOverlays（AutoBuild=false 或无节点项则直接 done 并移除）。
    - worker：`runProfileChangeWorker(ctx)` 单次处理循环——对所有非 done 记录顺序执行 publish→adopt→逐项（记录内节点顺序、system 先 runtime 后）构建；构建前镜像存在且新于 nodes.conf mtime 则直接置 done；否则调用 `overlay.BuildOverlay(mergedNode, allNodes, context, names)`（经可替换 hook 便于注入失败）；成功置 done、失败置 failed+错误+Attempts++，继续其他项；全部项 done 后记录移除。处理结束若仍有待定项则保留 journal，等待下次触发。
    - 触发 API：`kickProfileChangeWorker()`（非阻塞，守护循环通过 `sync.Once` 启动并带 wake channel；同进程串行，重复触发合并）；`RecoverProfileChanges(ctx)` 供 daemon 启动/SIGHUP/nodaemon CLI 同步执行一轮处理。
    - 进程内提交互斥锁串行化 Begin→publish→adopt→kick。
    - 提供两个编排入口：`ApplyProfileChangeDaemon(roots, prev, next)`（API：publish→Reload→保存 overlays 阶段→kick，立即返回；发布/adopt 错误上抛）与 `ApplyProfileChangeCLI(roots, prev, next) error`（journal+publish 后调 `DaemonReload()`；nodaemon 时执行同步 `RecoverProfileChanges`；DaemonReload 失败时记录保留 phaseAdopt 并返回错误）。
    - 全过程 wwlog（roots、节点数、阶段迁移、每项成败与重试）。
- **Acceptance Criteria Addressed**: AC-3, AC-5, AC-6, AC-7, AC-8, AC-10, AC-13, AC-14
- **Test Requirements**:
  - `rule` TR-4.1: 缺失 image（root 直接引用与节点经嵌套继承两种情形）与缺失 overlay 均使 Begin 返回 `ErrInvalidProfileChange`，磁盘 nodes.conf 不变且无 journal 文件。证据：编排器单测。
  - `rule` TR-4.2: 注入原子写失败：记录停留 phasePublish、旧文件可读；二次调用恢复（移除注入）后发布成功并推进到 overlays 且 pending 项被构建。证据：测试输出与阶段断言。
  - `rule` TR-4.3: 注入 Reload 失败：记录停留 phaseAdopt；恢复后内存 DB 反映新值（通过 warewulfd.GetNode/查询验证），阶段进入 overlays 并完成。证据：测试输出。
  - `rule` TR-4.4: AC-7 四象限（n1 system failed / n1 runtime done / n2 system done / n2 runtime done）：故障模板 overlay 制造真实构建失败；修复模板后再跑恢复，全部 done 且 journal 文件删除。证据：测试输出。
  - `rule` TR-4.5: 重启续跑：手工写入含 pending 项的 phaseOverlays journal（镜像不存在），调用 `RecoverProfileChanges` 后镜像生成、已 done 项不重复构建、journal 删除；节点已删除的 pending 项被跳过并置 done。证据：测试输出。
  - `rule` TR-4.6: AutoBuild=false 的记录在 adopt 后直接 done 移除，全过程不调用构建 hook、不产生镜像文件。证据：测试断言。
  - `rubric` TR-4.7: 维度=阶段机内聚与失败路径完备性（AC-13 锚点）；阈值>=4；证据：API/CLI/server 仅薄接入的代码结构 + 失败注入矩阵全通过。
  - `rubric` TR-4.8: 维度=风格一致性（AC-14 锚点，wwlog/错误包装/测试风格）；阈值>=4；证据：评审与 `go vet ./...`。

## Task 5: API `updateProfile` 接入事务编排器

- **Status**: `completed`
- **Completion Evidence**:
  - 重写 [api/profile.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/warewulfd/api/profile.go) `updateProfile()`：`node.New()` 读 prev → `Clone()` 得 next → mergo 合并 → `ApplyProfileChangeDaemon`；不存在 profile 返回 404；`ErrInvalidProfileChange` 映射 400 `INVALID_ARGUMENT`；发布/adopt 错误上抛 500；成功响应仍为更新后的 profile JSON。add/delete 保持原状（透明获得原子写）。
  - journal store 改为按解析路径自动重载（[profilechange_journal.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/warewulfd/profilechange_journal.go)），消除跨 testenv 的全局路径粘性。
  - TR-5.1：既有 `profile_test.go` 7 用例（含 update 响应/结果 YAML）不修改全过。
  - TR-5.2：新增 [profile_change_test.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/warewulfd/api/profile_change_test.go) `TestProfileUpdateRebuildsAffectedNodes`：n1 经 a、n2 直接、n3 无关；PATCH 200 含 new-arg；n1 双镜像新建、n2 旧代镜像被覆盖（内容/mtime）、n3 标记镜像字节与 mtime 均不变、journal 清空、nodes.conf 含新参数。
  - TR-5.3：环与缺失 overlay 均 400/INVALID_ARGUMENT（含 ghost 名）、磁盘字节不变、无 journal；`TestProfileQueriesLeaveNoJournal` 验证 GET 不产生 journal。
  - 命令：`go test ./internal/pkg/warewulfd/api/` ok 2.4s。
- **Priority**: high
- **Depends On**: Task 4
- **Description**:
  - 重写 `internal/pkg/warewulfd/api/profile.go` 的 `updateProfile()`：
    1. `node.New()` 读取 prev；对其做深拷贝得到 next（yaml 或 gob round-trip，注意保留可解析性）；
    2. 在 next 上对目标 profile 执行与现状一致的 mergo `MergeWithOverwrite`；profile 不存在 → 404；
    3. 调 `warewulfd.ApplyProfileChangeDaemon([]string{input.ID}, prev, next)`：`ErrInvalidProfileChange` 与现有直接引用校验错误一律映射 400 `INVALID_ARGUMENT`；发布/adopt 错误映射 500；
    4. 成功响应仍返回 next 中该 profile（`*output`）JSON，字段与现状一致；overlay 构建结果不进入响应体。
  - 保留 addProfile/deleteProfile 现状（仅透明获得原子写）。
- **Acceptance Criteria Addressed**: AC-2, AC-3, AC-9, AC-11, AC-12
- **Test Requirements**:
  - `rule` TR-5.1: 既有 `profile_test.go` 全部用例（含 "update a profile" 响应 JSON 与结果 YAML 断言）不修改即通过。证据：`go test ./internal/pkg/warewulfd/api/ -run TestProfileAPI -v`。
  - `rule` TR-5.2: 新增 API 集成用例：n1 经 a 引用 base、n2 直接引用、n3 无关；PATCH base 的 kernel args → 200；等待 worker 空闲后 n1/n2 system/runtime 镜像 mtime 更新、n3 不变；journal 已清空。证据：httptest 用例输出。
  - `rule` TR-5.3: 新增用例：环/缺失引用 → 400 且 nodes.conf 字节不变、无 journal；GET /api/profiles 不产生 journal 文件。证据：测试输出。

## Task 6: CLI `wwctl profile set` / `unset` 接入事务编排器

- **Status**: `completed`
- **Completion Evidence**:
  - [set/main.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/app/wwctl/profile/set/main.go) 与 [unset/main.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/app/wwctl/profile/unset/main.go)：加载 DB 后立即 `Clone()` 快照 prev；确认后以实际 diff map 的键（排序）为 roots 调 `warewulfd.ApplyProfileChangeCLI`，取代直接 Persist+DaemonReload；校验失败不落盘、非零退出。
  - 实现期澄清并回写 spec FR-2/FR-3：校验针对**本次新引入**的引用（新增的 profile 边/镜像/overlay；prev 无环→next 有环才拒），存量悬空引用不阻断无关字段编辑（AC-12 原文即"将引入"；同时满足 AC-11 既有 `unset comment but keep image` 等测试不修改即通过）。
  - TR-6.1：`go test ./internal/app/wwctl/profile/...` add/set/unset/list 全部 ok（set 2.1s、unset 4.8s）。
  - TR-6.2：新增 [profile_change_set_test.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/app/wwctl/profile/set/profile_change_set_test.go)：嵌套变更 n1 双 context 镜像重建+journal 清空；引入环（`--profile=a` 于 base 经 a→base）报错含 cycle、磁盘不变、无 journal；引入缺失 overlay 报错含 ghost 且不落盘。3/3 PASS。
- **Priority**: high
- **Depends On**: Task 4
- **Description**:
  - set/unset 现有流程到"用户确认"为止保持不变；在确认之后：
    1. 修改前保留 prev 深拷贝（在加载 DB 后、变更前制作）；
    2. roots = 实际产生 diff 的 profile 集合（两命令已有 `profileChanges` map，直接取键）；
    3. 无 diff 维持现有 "No changes" 返回；
    4. 以 `warewulfd.ApplyProfileChangeCLI(roots, prev, &nodeDB)` 取代直接 `Persist()`+`DaemonReload()`；校验失败（环/缺失引用）返回错误、不写盘；DaemonReload 失败返回错误（set 现状为上抛；unset 现状为 Warn，保持各自风格但阶段已持久化）。
  - nodaemon（测试环境）下由编排器在进程内同步完成恢复，镜像重建立即可观察。
- **Acceptance Criteria Addressed**: AC-2, AC-3, AC-11, AC-12
- **Test Requirements**:
  - `rule` TR-6.1: set/unset 既有测试全部不修改即通过。证据：`go test ./internal/app/wwctl/profile/...`。
  - `rule` TR-6.2: 新增 CLI 用例：合法嵌套变更（SetNoDaemon）后节点 overlay 镜像被重建、journal 清空；引入环/缺失引用时命令出错、nodes.conf 不变、无 journal。证据：测试输出。

## Task 7: daemon server 启动与 SIGHUP 恢复接线

- **Status**: `completed`
- **Completion Evidence**:
  - 导出 `warewulfd.KickProfileChangeWorker()`；[server.go](file:///Users/ding/Documents/swe/0910073/project-05/internal/pkg/warewulfd/server/server.go) `RunServer` 初始 Reload 后唤醒 worker（恢复上次 daemon 未完成记录），SIGHUP 处理在 Reload 后同样唤醒（接管 CLI 经 service reload 提交的 adopt/overlays 阶段），reload 错误打 Warn 不阻塞恢复；信号外行为未改。
  - TR-7.1：新增 `TestKickProfileChangeWorkerDrainsJournal`（phaseOverlays 残留记录经异步 worker 在 10s 内清空、镜像落盘、journal 删除），PASS；Task 4 恢复用例已覆盖 adopt/publish 阶段恢复。
  - TR-7.2：`go build ./internal/pkg/warewulfd/server/` 通过。
- **Priority**: high
- **Depends On**: Task 4
- **Description**:
  - 修改 `internal/pkg/warewulfd/server/server.go`：初始 `warewulfd.Reload()` 之后启动 profile 变更 worker（`kickProfileChangeWorker`/等价启动函数）；SIGHUP 处理中 `warewulfd.Reload()` 之后触发一轮恢复（唤醒 worker），使 CLI 提交并 SIGHUP 的记录从 adopt 阶段继续。
  - 不改变信号处理之外的任何 server 行为。
- **Acceptance Criteria Addressed**: AC-8, AC-10
- **Test Requirements**:
  - `rule` TR-7.1: 构造 phaseAdopt/phaseOverlays 残留 journal，模拟启动路径调用的恢复函数后，记录被推进至清空（单元级覆盖恢复函数；server 包保持薄接线）。证据：warewulfd 包恢复测试。
  - `rule` TR-7.2: `go test ./internal/pkg/warewulfd/server/...` 既有用例通过（若该包无测试则以 `go build` + 接线评审为证）。证据：构建/测试输出。

## Task 8: 端到端验证与全量回归

- **Status**: `completed`
- **Completion Evidence**:
  - 损坏 journal 韧性用例 `TestProfileChangeCorruptJournalRecovery`：恢复返回解析错误但不 panic、`Reload()` 仍成功、删除坏文件后恢复干净。
  - TR-8.1：容器 golang:1.25（+libgpgme-dev/cpio）内 `go build ./...` 零错误、`go vet ./...` 零告警、`gofmt -l internal/ cmd/` 无输出。
  - TR-8.2：`go test ./...` 141 个包（93 ok + 48 无测试文件），grep FAIL/panic 为空，全量绿；无既有失败需要甄别。
  - TR-8.3 rubric 5/5：AC-1~14 自测矩阵全部有对应自动化证据；两处实现期偏差（Reload 签名增强、FR-2/3"新引入引用"精确化）已回写 spec 并保留既有行为兼容。
- **Priority**: high
- **Depends On**: Task 5, Task 6, Task 7
- **Description**:
  - 补齐跨 AC 的集成测试矩阵（在 warewulfd 包或 api 包测试中）：完整成功代际刷新、未受影响节点 mtime 不变、三阶段失败注入与恢复、重启续跑、autobuild=false、手动 `overlay.BuildOverlay`/镜像新于 nodes.conf 时恢复直接置 done、journal 损坏文件不阻断 daemon reload（仅错误日志）。
  - 运行 `go vet ./...`、`go build ./...`、`go test ./...`；甄别并记录任何与本改动无关的既有失败。
- **Acceptance Criteria Addressed**: AC-1..AC-14
- **Test Requirements**:
  - `rule` TR-8.1: `go build ./...` 与 `go vet ./...` 零新增告警/错误。证据：命令输出。
  - `rule` TR-8.2: `go test ./...` 全绿；如有环境性既有失败，逐一甄别并在任务证据中记录与本改动无关的依据。证据：全量测试输出。
  - `rubric` TR-8.3: 维度=整体方案与 spec 符合度（AC-13/AC-14 综合）；1-5；阈值>=4；证据：自测矩阵逐条对照 spec AC。
