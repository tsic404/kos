# KOS

KOS 是运行在 Kubernetes 上的 homelab 操作系统。本仓是 **kos-shell** 的实现：内核对象模型、
归属标记、扩展接口与 ABI 的实现骨架。设计仓（意图与承诺）为
<https://gitea.tsic.top/tsic/kos-design>，契约以 `docs/02-contracts.md` 为准。

当前阶段交付内核对象模型、归属三态、写入路径、ABI、控制面 API、通用调和内核、
扩展装载（包摘要、声明、九类贡献、依赖激活与可见性投影）、应用渲染与本地账号会话；
不含升级回滚、停用与卸载、各贡献类自身的语义与 Provider 能力验证。

## 布局

| 路径 | 内容 |
|---|---|
| `api/` | KOS API 组标识、十类内核对象的封闭集合、双版本 scheme |
| `api/v1alpha1/` | 第一版对象类型与状态形状 |
| `api/v1beta1/` | 第二版对象类型；与 v1alpha1 同结构，是存储版本 |
| `config/crd/` | 由 Go 类型生成的 CRD 清单（`make generate`） |
| `pkg/markers/` | C-02 归属标记的键、载体与校验 |
| `pkg/abi/` | ABI 自检：kind 集合、作用域、双版本同结构、无 webhook |
| `pkg/state/` | C-07/C-08 状态轴：`Reconciled` 三值、失败条件与健康聚合 |
| `pkg/reconcile/` | C-06 通用调和循环：去重工作队列、单轮时间预算与指标 |
| `pkg/controlplane/` | 控制面 HTTP 面：资源路由、投影、统一错误包络、计划请求 |
| `pkg/write/` | 写入路径：C-03/C-04 写入者分配表、准入判定、服务端合并提交与 409 冲突 |
| `pkg/app/` | C-11/C-32/C-33 应用域：配置结构、翻译模板、能力比较、生命周期与动作记录 |
| `pkg/app/builtin/` | 发行版内置的五份应用定义，摘要由内容计算 |
| `pkg/identity/` | 账号与会话：本地账号、锁定计数、一次性口令、会话边界与失效、自身数据库与每日导出 |
| `pkg/adoption/` | 归属动作：接管、释放、放弃与接管前的不可变快照 |
| `pkg/serial/` | 调和循环之外显式写入动作的按对象串行门（调和轮次由 `pkg/reconcile` 串行） |
| `pkg/extension/` | C-17 至 C-20/C-28：包摘要、声明、九类贡献、依赖激活、可见性投影与贡献类控制器 |
| `tests/acceptance/TC-27/` | 读投影带观测时间，超阈值标记陈旧 |
| `tests/acceptance/TC-28/` | 同一写请求重复提交只产生一个对象 |
| `tests/acceptance/TC-29/` | 四类失败的错误包络，断线后操作仍到终态 |
| `tests/acceptance/TC-35/` | 计划请求无副作用，计划与执行一致 |
| `tests/acceptance/TC-104/` | 同一对象双版本读取的字段一致性 |
| `tests/acceptance/TC-105/` | 转换策略为无转换、集群无准入 webhook |
| `tests/acceptance/TC-25/` | `Reconciled` 的 True、False、Unknown 三值 |
| `tests/acceptance/TC-26/` | 重复调和无增量、超限单轮可被指标观察到 |
| `tests/acceptance/TC-17/` | 接管、释放与放弃 |
| `tests/acceptance/TC-18/` | 父对象调和器不写子对象 spec |
| `tests/acceptance/TC-19/` | 归属标记的载体 |
| `tests/acceptance/TC-24/` | 并发写入与角色写权限 |
| `tests/acceptance/TC-23/` | 内置定义安装到 Ready、十类对象可被发现 |
| `tests/acceptance/TC-45/` | 同一配置的翻译结果一致、等价配置等价 |
| `tests/acceptance/TC-46/` | 私有配置里的卷与入口字段被拒 |
| `tests/acceptance/TC-47/` | 未知注解不改变阶段、健康与下游 |
| `tests/acceptance/TC-54/` | 缺失能力与缺失命名空间停在 Pending，齐备后 Installing 再到 Ready |
| `tests/acceptance/TC-55/` | 五次重试预算、退避上限与用户重试 |
| `tests/acceptance/TC-56/` | 卸载阻塞可见、orphan 门槛与应用弃置清单 |
| `tests/acceptance/TC-72/` | 自身数据库的每日导出与离线恢复 |
| `tests/acceptance/TC-117/` | 登录失败同一结果、按登录名锁定与退出 |
| `tests/acceptance/TC-118/` | 角色变更、改口令、停用与删除前置转移 |
| `tests/acceptance/TC-1/` | 安装与卸载产品不改变内核 kind 集合；对象类只在其 API 组 |
| `tests/acceptance/TC-2/` | 能力基线不变；扩展能力只在安装期间可读 |
| `tests/acceptance/TC-3/` | 第一方与第三方同类贡献装载、权限判定与卸载一致 |
| `tests/acceptance/TC-4/` | 九类声明逐项可见，组件状态就绪 |
| `tests/acceptance/TC-5/` | 未声明与未安装的贡献不可见，越界写入被拒绝 |
| `tests/acceptance/TC-6/` | 依赖缺失时无贡献生效；一个控制器失败只影响它管理的对象 |

## 对象模型

API 组为 `kos.tsic.top`。十类对象：Application、ApplicationDefinition、Volume、Endpoint、Provider、
SystemComponent、BackupPolicy、RestoreJob、NodeDevice、VolumeMigration。其中
ApplicationDefinition、Provider、SystemComponent、NodeDevice 为集群作用域，其余为命名空间作用域。

ApplicationDefinition 与 NodeDevice 没有 status 写者，因此没有 status 字段与 status 子资源；
其余八类带 `status.phase`、`status.health.state` 与 `Reconciled` 条件。

`spec` 字段由契约逐个加入：尚未固定字段的对象保持空 spec，实现不发明线格式。当前已兑现
C-60 的 `spec.owner`（五类带所有者的对象）、C-59 的 `Application.spec.visibility`、
C-17 的 `SystemComponent.spec.package`（工件引用与 sha256 摘要）、
C-32 的 `Application.spec.definitionRef`/`namespace`/`config`/`volumes` 与定义侧
`spec.package`/`config`/`template`/`hooks`/`chart`/`requires`，以及 C-33 的
`status.retryCount`、`status.nextRetryAt`、`status.uninstall`。

## ABI

- 同一时刻服务 `v1alpha1` 与 `v1beta1`，存储版本为 `v1beta1`；两版逐字段同结构，转换策略为
  `None`，不存在转换 webhook。
- `api/v1beta1` 是 `api/v1alpha1` 的结构副本，双份类型是刻意的：C-16 要求相邻版本同结构，
  复制让差异在代码里可见，`pkg/abi` 与 `config/crd` 的自检让它无法悄悄漂移。
- 扩展对象类属于扩展自己的 API 组，不得出现在 `kos.tsic.top`（C-19）。

## 标记

`pkg/markers` 固定 C-02 的九个键：归属（`managed-by`、`owner-uid`、`adopted-by`、
`adoption-reason`）、放弃（`declined`）、释放（`released`）、有效性（`invalid`、`stale`）。
除 `adopted-at` 用注解外一律用标签；未列入的注解不改变行为。

## 写入路径

`pkg/write` 把 C-03/C-04 的写入者分配写成表：主体、对象类与对象部位（`spec`、`status`、
标记）三者查一张表，表外即拒绝，拒绝信息带上主体与对象。用户写入走服务端合并提交，字段
管理器按主体区分，同一字段集的第二个写者拿到 409 与当前对象；不相交字段集都保留。

`pkg/adoption` 是 C-02 的三个动作。接管写入归属标记，并在 `kos-system` 留下一条
`immutable` 的 ConfigMap 记录，内容等于接管前 spec；释放移除归属标记，对象保持当前状态且
KOS 不再写它；放弃写入 `declined`，对象留在 unmanaged，不再被列为可接管对象。

`pkg/serial` 是显式门：调和循环之外的读—改—写动作（接管、释放、放弃）按对象键串行；
调和轮次自身由 `pkg/reconcile` 的去重队列保证同一对象同时最多一轮，两层不共用锁，
跨层并发写由服务端冲突判定收口：服务端合并提交按字段集、显式更新按 `resourceVersion`，
都返回 409 与当前对象（C-05）。

## 控制面 API

浏览器与扩展只有这一个读写入口（C-12），控制面自己持有集群客户端，不把凭据交给调用方。

| 方法 | 路径 | 语义 |
|---|---|---|
| `GET` | `/api/v1/resources/{kind}` | 一页投影；`limit`、`continue`、`labelSelector`、`namespace` |
| `GET` | `/api/v1/resources/{kind}/{name}` | 单对象投影 |
| `POST` | `/api/v1/resources/{kind}` | 创建；202 已接受 + 对象引用 |
| `PUT` | `/api/v1/resources/{kind}/{name}` | 更新客户端所有的字段；需 `If-Match` 或 `metadata.resourceVersion` |
| `DELETE` | `/api/v1/resources/{kind}/{name}` | 删除；需 `If-Match` |
| `POST` | `/api/v1/plan-requests` | 三类差异、摘要与警告；不写任何对象 |
| `POST` | `/api/v1/apply-requests` | 接受期望对象集后执行；进度从对象本身读取 |
| `POST` | `/api/v1/action-requests` | 应用动作 `retry`/`orphan`；202 已接受 + 动作记录引用 |

- 读投影每项带 `observedAt` 与 `stale`；陈旧阈值是 `controlplane.StaleThreshold`（2 分钟），所有来源共用。
- 写请求返回 202 与对象引用，不返回已完成；工作在接受后继续，客户端断线不影响推进。
- 同一写请求重复提交收敛到同一对象；已存在且期望状态不同时报 `object_exists`。
- 写请求带 `If-Match`（或 `metadata.resourceVersion`）；版本已变时由 API server 判冲突，返回 409
  与当前对象 `current`，不静默覆盖。
- 分页游标绑定对象类型、命名空间与过滤条件；不属于本次查询的游标被拒绝，不会静默漏数据。
- 失败响应统一为 `{"error": {...}}`，含 `code`、`message`、`stage`、`object`、`retryable`、
  `requiresUserAction`、`sideEffects`。`code` 在一个主版本内不变。
- 内核十类之外，已激活组件声明的扩展对象类也由控制面服务；扩展对象类用 `apiVersion`
  （读、删）或请求体的 `apiVersion`（写）指名 API 组。未声明或未安装的类返回
  `capability_missing`（C-19、F16），对象类只在组件激活时被服务。
- `GET`、`GET {name}`、`DELETE` 的扩展对象类通过 `apiVersion` 查询参数指名其 API 组与版本。
- 应用写入在落库前校验配置：`spec.config` 的键必须由定义的配置结构声明，卷尺寸、存储策略、
  主机名与端口由 Volume/Endpoint 对象持有，写进私有配置返回 `invalid_field` 并指出冲突字段（C-32、F3）。
- 动作请求写成 `kos-system` 的一次性动作记录；应用调和器读取后删除。`retry` 只接受 Failed
  的应用，`orphan` 只接受阻塞超过十分钟且阻塞清单非空的 Uninstalling 应用。

## 扩展装载

`pkg/extension` 把 C-17 至 C-20 与 C-28 写成一条装载流水线：

- **安装请求**：`SystemComponent.spec.package` 给出工件引用与 sha256 摘要；`Installer` 读取
  集群里带归属标记的组件，经 `ArtifactSource` 取回工件，摘要不匹配即拒绝。同一扩展标识
  同时只保留最先安装的版本；adopted 组件不携带包，也不贡献对象类、页面或控制器。
- **声明**（C-18）：标识、版本、目标框架版本、九类贡献（对象类、控制器、能力、页面与动作、
  设置项、目录来源、发现适配器、诊断适配器、偏好结构）以及依赖（能力与组件，均带版本范围）。
  目标框架版本随声明装载，兼容判定属于独立契约。
- **激活**（C-28）：按依赖求不动点，只有能力基线与已激活组件能满足的依赖才算满足。依赖不满足
  时组件不可用、状态列出缺失项，且没有任何贡献生效；某一贡献类校验失败只让该类缺席，组件
  降级，其余类照常生效。
- **可见性投影**：`Registry` 只回答已激活声明里的贡献，未声明的一律不出现；能力清单是 C-09
  的八项基线加扩展能力（扩展能力名带扩展标识前缀）；页面按会话角色给出可见性判定；对象类
  解析供控制面判断某个扩展类是否正在被服务。
- **控制器**（C-20）：`ControllerSet` 为每个声明的控制器启动一条 `pkg/reconcile` 循环，
  连续失败达到阈值即判为不可用；只有该控制器管理的对象投影为 Unknown，其他扩展与内核对象
  保持自己的状态。声明里的写入范围经 `WriteScope.Admit` 转成写入路径检查的分配。
- **组件状态**：`Installer` 以组件调和者的身份写 `status`，含 `phase`、`health` 与
  `PackageVerified`、`DependenciesMet`、`ContributionsLoaded` 三个条件；状态未变化时不写。

停用、卸载、框架版本兼容与各贡献类自身的语义不在本阶段实现，分别属于独立契约。

## 调和与状态轴

`pkg/reconcile` 提供通用循环：请求只携带对象名，每轮读取当前事实；队列对同一对象去重，
同一对象的调和因此串行。每轮有固定时间预算（C-06 的上限），以轮次上下文的 deadline
下发；调和器必须在预算内返回，忽略 ctx 的调和器会阻塞该 worker，直到它返回。超过预算的
轮次计入 `kos_reconcile_round_over_limit_total`，每轮耗时记录在
`kos_reconcile_round_duration_seconds`，panic 的轮次计入
`kos_reconcile_round_panics_total` 并作为失败重试。长时间动作应由调和器创建 Job 后返回，
由下一次观察推进，不在轮内同步等待。返回错误按退避重试；`RequeueAfter` 安排下一次观察。

`pkg/state` 提供两条正交状态轴。`Reconciled` 条件取值 True（期望状态已完整应用）、
False（仍有待处理动作或被阻塞，`reason` 给出原因）与 Unknown（无法观察）；没有条件即
Unknown，表示尚无写者观察过该对象。失败由对象自己的失败条件表达，不并入
`Reconciled`。`EvaluateHealth` 按 Unknown、Unavailable、Degraded、Healthy 的优先级
求值：无法观察不推断为 Healthy，期望副本大于零且就绪为零、必需依赖不可用或必需卷丢失
判 Unavailable，部分就绪、非必需依赖降级、下游漂移或能力保证不足判 Degraded；暂停与
维护停止按有效期望副本零判定。

## 应用渲染与生命周期

`pkg/app` 把 C-32 的包结构与 C-33 的生命周期落成可执行的规则。定义的配置结构是界面字段的
唯一来源：类型、默认值、必填、范围、枚举与敏感标记都在这里声明，提交的值在写入前被解析、
补默认值并按类型归一，因此显式写出默认值与省略它渲染出同样的结果。翻译模板是纯函数：
只读应用名、声明命名空间、解析后的配置与卷声明名，不访问外部服务、不使用随机数，同一输入
产生同一输出。渲染产物必须落在应用声明的命名空间；包声明存储类、自带卷模板或渲染
PersistentVolumeClaim 都被判为非法包。

应用调和器按 C-33 推进阶段。缺定义、缺必需能力或缺声明命名空间时停在 `Pending` 并写出对应
原因与失败条件；前置齐备后先写 `Installing`，下一轮渲染并写入声明命名空间内的全部下游对象，
随后进入 `Ready`。可重试失败在五次预算内保持 `Installing`，`retryCount` 递增、`nextRetryAt`
给出下一次尝试时刻，退避从 10 秒起指数增长、上限 5 分钟；第五次失败进入 `Failed`，不再自动
重试。用户经控制面提交 `retry` 动作重置预算并回到 `Installing`。

应用调和器在应用对象上放自己的 finalizer 作为卸载门：删除请求到达后进入 `Uninstalling`，
逐个删除渲染过的下游对象并保留卷与 PVC；仍有对象存在时把阻塞清单、起始时刻与是否超过
10 分钟的 `stalled` 写进状态，投影因此能显示卡住与阻塞对象。`orphan` 动作放弃这些下游对象、
释放卸载门，结果由应用调和器写入 `kos-system` 的一份不可变应用弃置清单，而不是写进即将
消失的应用状态；清单在应用对象消失后仍可读。

健康从**已存储**的对象求值：渲染出的期望文档没有 status，读它会把每个工作负载
应用判成 Unavailable。内核没有 watch 源，阻塞与已就绪的应用都按有界间隔重新观察。

C-11 的能力需求比较按固定规则执行：先取满足版本范围的最高版本，再逐项比较保证；必需能力
完全缺失停在 `Pending`，能力存在但保证不足时照常安装并把 `能力.保证键` 列进失败条件与
Degraded 健康。能力来源是 `pkg/app.CapabilitySource`；Provider 注册（C-47）尚未接入时默认为空。

## 账号、会话与自身数据库

`pkg/identity` 实现 C-63 的本地账号与会话，以及 C-61 的控制面自身数据库。浏览器身份只有本地账号；
Kubernetes 凭据不进入浏览器。

- 登录：账号不存在、口令错误与账号停用返回同一错误（`login_failed`）；计数键是登录名，连续五次失败后
  该名字锁定五分钟，`retryAfterSeconds` 给出剩余时间；成功登录与锁到期都清零。计数行在十五分钟无进展后
  由下一次失败清理，未认证的登录路径不会让这张表无限增长。
- 会话：空闲十二小时与绝对七天取先到者，响应可读剩余秒数。退出、改口令、停用与删除立即结束相应会话；
  改口令保留当前会话、作废其余会话。角色不写入会话，每次请求按账号当前角色求值。
- 账号：由 Owner 创建、停用、启用、改角色与删除；账号以一次性口令创建，首次登录必须先改口令
  （`password_change_required`），此前只放行改口令、退出与会话读取。最后一个可用 Owner 不能被停用、
  降级或删除；删除前必须转移其拥有的应用、卷、备份策略、恢复作业与卷迁移，未转移时返回
  `account_has_objects` 与 `ownedObjects`。
- 口令只保存 argon2id 单向摘要，会话只保存令牌摘要；日志、响应与投影响应都不含口令。
- 数据库是单文件 SQLite（纯 Go 驱动），存放用户、会话、偏好与审计，不在调和路径上。schema 由内嵌迁移
  脚本按版本前滚，`Rollback` 反向执行最新一版。数据库不可用时登录与写请求失败
  （`database_unavailable`），已登录会话的读请求从内存副本照常提供。
- 每日导出用 `VACUUM INTO` 把一致性快照写到配置的备份目标；未配置目标时跳过并在 `Exporter.Status`
  留下可观察提示。恢复是离线动作：控制面停机后用 `Restore` 以导出文件替换数据库文件，再启动；
  账号、偏好与审计回到导出时刻。

| 方法 | 路径 | 语义 |
|---|---|---|
| `POST` | `/api/v1/auth/login` | 登录；返回令牌与剩余有效期 |
| `POST` | `/api/v1/auth/logout` | 退出，结束当前会话 |
| `GET` | `/api/v1/auth/session` | 当前身份与剩余有效期 |
| `PUT` | `/api/v1/auth/password` | 改自己的口令；其余会话作废 |
| `GET` | `/api/v1/accounts` | 账号列表（Owner） |
| `POST` | `/api/v1/accounts` | 创建账号并返回一次性口令（Owner） |
| `PUT` | `/api/v1/accounts/{name}` | 改角色或停用/启用（Owner） |
| `DELETE` | `/api/v1/accounts/{name}` | 删除账号；仍有归属时拒绝（Owner） |

除登录外，所有路由都要求 `Authorization: Bearer <token>`。`identity.Authorizer` 把 `pkg/write`
的角色规则接到控制面 API：Guest 只读，写请求按会话当前角色判定。

## 构建与测试

```bash
make generate   # 重新生成 deepcopy 与 CRD，并保持与 Go 类型一致
make verify     # 生成物无漂移 + gofmt + go vet
make test       # 单元测试 + 验收测试（自动下载 envtest 二进制）
make acceptance # 只跑 tests/acceptance，输出用例名
```

验收测试使用 envtest 启动真实 kube-apiserver 与 etcd，安装 `config/crd` 后读写对象；
环境变量 `KUBEBUILDER_ASSETS` 指向 envtest 二进制目录，`make test` 会下载并设置。
