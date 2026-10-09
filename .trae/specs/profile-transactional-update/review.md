# Independent Review - Profile 事务化更新与派生结果协调重建

- **Review R1**: pending（由全新上下文的独立 reviewer 执行）
- **Checkpoints**: CP-R1 规则 AC、CP-U1 可用性 rubric、CP-F1 失败注入矩阵、CP-C1 兼容性回归、CP-S1 规格偏差

## 自测 AC 对照（实现方，供 reviewer 复核）

| AC | 类型 | 证据 |
|---|---|---|
| AC-1 反向闭包/受影响节点 | rule | node/dependency_test.go `TestProfileReverseClosure`/`TestAffectedNodes`：{a,b,base}、{n1,n2} |
| AC-2 循环写盘前拒绝 | rule | node 单测（自环/双/三节点环）+ API 400 用例 + CLI `--profile=a` 用例；磁盘/ journal 不变断言 |
| AC-3 镜像/overlay 可用 | rule | warewulfd `TestProfileChangeValidationRejectsBadChange`（root overlay + 嵌套继承 image）+ API ghost + CLI ghost |
| AC-4 覆盖/否定/无关排除 | rule | node `TestAffectedNodesOverridesAndNegation`（image 标量覆盖、~base 嵌套否定、tags 包含）；API n3 镜像字节+mtime 不变 |
| AC-5 原子发布可恢复 | rule | util/atomicfile_test + node `TestPersistToFileAtomicFailureKeepsOldVersion` + 编排器 publish 失败恢复 |
| AC-6 adopt 阶段独立 | rule | `TestProfileChangeAdoptFailureRecovery`：hook reload 失败→phaseAdopt，恢复后 GetNode 可见新值 |
| AC-7 节点×context 失败隔离 | rule | `TestProfileChangeOverlayFailureIsolation` 四象限 + 修复模板后收敛 |
| AC-8 重启续跑 | rule | `TestProfileChangeRestartResume`（崩溃记录、已删除节点跳过、done 不重建） |
| AC-9 API 端到端 | rule | api `TestProfileUpdateRebuildsAffectedNodes`（200/响应 JSON/YAML/三节点镜像） |
| AC-10 autobuild=false | rule | `TestProfileChangeAutobuildDisabled`（0 构建、无镜像、journal 清空） |
| AC-11 既有入口兼容 | rule | `go test ./...` 141 包全绿，既有断言零修改；add/delete/node 端点仅获原子写 |
| AC-12 CLI 校验前置 | rule | 既有 set/unset 全过 + 3 个新 CLI 事务用例 |
| AC-13 编排器质量 | rubric | 待 reviewer 评分（阈值 4） |
| AC-14 风格一致性 | rubric | 待 reviewer 评分（阈值 4） |

## 实现期偏差记录

1. **FR-2/FR-3 精确化为"新引入"语义**：校验仅拦截本次更新新引入的环（prev 无环→next 有环）与新引用（prev 不存在而 next 出现的 profile 边/镜像/overlay，root 级与节点合并级均做差集）；存量悬空引用不阻断无关字段编辑。依据 AC-12 原文"将引入环或缺失引用"与 AC-11（既有 `unset comment but keep image` 测试不修改即通过）。spec 已回写。
2. **`warewulfd.Reload()` 改为返回 error**：旧调用点以语句形式忽略返回值，全量构建确认兼容。
3. 校验语义在 API 侧也由"全量直引校验"放宽为"新引入校验"（同一编排器），既有 API 测试不受影响。

## 变更文件清单

新增：
- internal/pkg/node/dependency.go (+dependency_test.go)
- internal/pkg/util/atomicfile.go (+atomicfile_test.go)
- internal/pkg/node/modifiers_test.go
- internal/pkg/warewulfd/profilechange.go
- internal/pkg/warewulfd/profilechange_journal.go (+_test.go)
- internal/pkg/warewulfd/profilechange_test.go
- internal/pkg/warewulfd/profilechange_worker_test.go
- internal/pkg/warewulfd/api/profile_change_test.go
- internal/app/wwctl/profile/set/profile_change_set_test.go

修改：
- internal/pkg/node/constructors.go（Clone）
- internal/pkg/node/modifiers.go（原子 PersistToFile）
- internal/pkg/warewulfd/nodedb.go（Reload 返回 error）
- internal/pkg/warewulfd/api/profile.go（updateProfile 接入）
- internal/app/wwctl/profile/set/main.go、unset/main.go（ApplyProfileChangeCLI）
- internal/pkg/warewulfd/server/server.go（启动/SIGHUP 恢复）

## 验证命令（Linux 容器，macOS 宿主无法直接编译）

```
docker run --rm -v "$PWD":/src -v ww-gomod:/go/pkg/mod -w /src ww-golang:1.25 \
  sh -c 'go build ./... && go vet ./... && go test ./...'
```
（镜像 ww-golang:1.25 = golang:1.25 + libgpgme-dev/pkg-config/libbtrfs-dev/libdevmapper-dev/cpio）

## R1 发现与修复（已闭环）

- **F1（major，已修复）**：journal store 改为按文件身份（存在性/mtime/size）校验缓存，外进程 temp+rename 发布在下一次访问即可见；外进程删除同样感知；save 后刷新身份。新增 `TestProfileChangeJournalExternalWriter`（外写可见/本地 add 合并不覆盖/外删可见）与 `TestProfileChangeExternalProcessRecordRecovered`（预热空缓存后外进程 phaseAdopt 记录经 Recover 完成 adopt+构建）。
- **F2（minor，已修复）**：新增导出 sentinel `warewulfd.ErrDaemonReload`；unset 对该错误恢复历史"告警但不失败"风格（set 仍上抛），更新 tasks.md 描述一致。
- **F3（minor，已修复）**：新增 `persistRecord()`，所有 overlays/阶段错误路径的 journal 更新失败均 `wwlog.Error` 上报（共 9 处）。
- **F4（nit，已修复）**：移除未使用的 `phaseDone`，阶段注释改为 publish/adopt/overlays + 完成即删除。
- **F5（nit，已修复）**：`AffectedNodes` next 侧合并失败改为 `wwlog.Warn`（不再静默）。
- **F6（测试缺口，已补齐）**：外进程写+缓存预热恢复（F1 配套）、`TestProfileChangeDoneItemsNotRebuilt`（持久化 stDone 零构建、pending 正常构建）、`TestProfileChangeBuildCountExcludesOverrideAndNegation`（覆盖/否定节点构建调用计数=0，实际变更节点=1×2）。
- **F7（nit，不修）**：崩溃残留 `.tmp-*` 回收——跨进程并发下回收不安全，spec 未要求；保持现状。
- 修复后回归：`go build ./...`、`go vet ./...`、`gofmt -l` 干净；`go test ./...` 零 FAIL/panic；`go test -race`（warewulfd/api/node/util/profile set/unset）全部 ok。

## Review History

### R1
- **Result**: FAIL
- **Checkpoints**: CP-R1 AC-1..12 证据基本真实（AC-8/AC-12 的跨进程承诺落空）；CP-U1 3/5；CP-S1 5/5
- **Findings**: F1 major（跨进程 journal 缓存）；F2 minor（unset 退出码回归）；F3 minor（journal 更新错误静默）；F4 nit（phaseDone 死常量）；F5 nit（合并失败静默）；F6 nit（测试缺口）；F7 nit（临时文件残留，不修）
- **Action**: 见上"R1 发现与修复"；R2 复审全部修复项

### R2
- **Result**: **PASS**
- **Checkpoints**: F1-F6 修复逐条验证有效（代码行+测试+运行证据）；F7 不修补可接受；高风险扫描（锁序单向、worker 生命周期、nodaemon 双分支、autobuild=false、损坏 journal fail-closed、SIGHUP 顺序）无问题。
- **Scores**: CP-U1 5/5；CP-S1 5/5。
- **Residual nits**: N1（外进程恢复测试已应建议改为真正的 `util.AtomicWriteFile` 外写→Recover，复跑 PASS）；N2（mtime+size 同纳秒同字节理论漏判，落在 spec 明确排除的跨进程并发写范围，不改）；N3（unset Warn 分支 seam 不可注入无直接单测，5 行人工确认为准）。
- **Final regression**: `go build ./...` + `go vet ./...` 干净；`go test ./...` 93 ok 包 / 0 FAIL / 0 panic；关键包 `go test -race` 全 ok。
