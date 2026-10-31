# KOS

KOS 是运行在 Kubernetes 上的 homelab 操作系统。本仓是 **kos-shell** 的实现：内核对象模型、
归属标记、扩展接口与 ABI 的实现骨架。设计仓（意图与承诺）为
<https://gitea.tsic.top/tsic/kos-design>，契约以 `docs/02-contracts.md` 为准。

当前阶段交付内核对象模型、归属三态、写入路径、ABI、控制面 API、通用调和内核、
扩展装载（包摘要、声明、九类贡献、依赖激活与可见性投影）、扩展停用与卸载
（数据保留语义、遗留清单与依赖阻塞）、框架版本兼容与升级预检、应用渲染与生命周期动作
（停止、启动、重启、扩缩容与资源规格）、入口形态与证书状态、升级回滚与外部改动仲裁、
备份策略调和与恢复到新实例（含演练基线）、本地账号会话、发现适配器与设备事实、卷迁移的六阶段
状态机与回滚，以及设置域、用户偏好与目录来源三类贡献的自身语义；不含其余贡献类的语义与
Provider 能力验证。

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
| `pkg/app/` | C-11/C-32/C-33/C-34/C-35 应用域：配置结构、翻译模板、能力比较、生命周期、升级回滚、漂移策略与动作记录 |
| `pkg/app/builtin/` | 发行版内置的五份应用定义，摘要由内容计算 |
| `pkg/identity/` | 账号与会话：本地账号、锁定计数、一次性口令、会话边界与失效、自身数据库与每日导出 |
| `pkg/adoption/` | 归属动作：接管、释放、放弃与接管前的不可变快照 |
| `pkg/discovery/` | C-25 发现适配器运行：按声明的类别与范围只读上报，多来源索引 |
| `pkg/device/` | C-37 设备采集、越界校验、陈旧与回收，以及设备视图与可分配推导 |
| `pkg/serial/` | 调和循环之外显式写入动作的按对象串行门（调和轮次由 `pkg/reconcile` 串行） |
| `pkg/volume/` | C-38/C-39：卷的声明、绑定观察、扩容预检与删除保护 |
| `pkg/migration/` | C-40 卷迁移：六阶段状态机、目标声明与步骤 Job、回滚点与失败记录 |
| `pkg/endpoint/` | C-43 至 C-46：五种入口形态、唯一性台账、应用反查、证书状态与组件依赖边 |
| `pkg/extension/` | C-16 至 C-20/C-28/C-29/C-48：包摘要、声明、九类贡献、依赖激活、可见性投影、停用与卸载（数据保留、遗留清单）、框架版本兼容与升级预检、贡献类控制器与删除受阻门 |
| `pkg/settings/` | C-23 设置域投影：当前值、来源、修改入口与不可修改原因 |
| `pkg/catalogue/` | C-24 目录来源：条目结构、摘要校验、定义同步与来源缺失的升级路径 |
| `pkg/access/` | C-59/C-60 判定：可见性谓词、角色写权限、所有权与跨所有者引用，扩展对象按声明授权 |
| `pkg/backup/` | C-41/C-42 数据保护：策略调和与计划触发、失败域拒绝、四类基线项的全量计算、恢复到新实例与演练比对；复制与摘要交给工作进程，调和轮只观测结果 |
| `tests/acceptance/TC-66/` | 卷迁移六阶段完成，绑定换到新声明、应用不变 |
| `tests/acceptance/TC-67/` | 五个阶段各注入一次失败后回滚到源卷 |
| `tests/acceptance/TC-68/` | 清理只打释放标记，不删除源卷 |
| `tests/acceptance/TC-27/` | 读投影带观测时间，超阈值标记陈旧 |
| `tests/acceptance/TC-28/` | 同一写请求重复提交只产生一个对象 |
| `tests/acceptance/TC-29/` | 四类失败的错误包络，断线后操作仍到终态 |
| `tests/acceptance/TC-35/` | 计划请求无副作用，计划与执行一致 |
| `tests/acceptance/TC-104/` | 同一对象双版本读取的字段一致性 |
| `tests/acceptance/TC-105/` | 转换策略为无转换、集群无准入 webhook |
| `tests/acceptance/TC-25/` | `Reconciled` 的 True、False、Unknown 三值 |
| `tests/acceptance/TC-26/` | 重复调和无增量、超限单轮可被指标观察到 |
| `tests/acceptance/TC-17/` | 接管、释放与放弃 |
| `tests/acceptance/TC-11/` | 发现只读、接管显式与多来源并列 |
| `tests/acceptance/TC-81/` | 越界写入标无效且不参与调度推导 |
| `tests/acceptance/TC-82/` | 设备事实陈旧与节点消失后的回收 |
| `tests/acceptance/TC-83/` | 设备占用来源与空闲设备 |
| `tests/acceptance/TC-131/` | 同一序列号的重复条目只保留最新 |
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
| `tests/acceptance/TC-50/` | 预检通过后应用新清单，回到 Ready 并记录升级完成 |
| `tests/acceptance/TC-51/` | 升级钩子非零退出时重新应用上一份清单并记录回滚 |
| `tests/acceptance/TC-52/` | 声明数据格式变更的升级被拒绝，阶段与下游不变且无绕过路径 |
| `tests/acceptance/TC-53/` | 外部改动强制暂停调和，错误摘要无效，三者一致后恢复 |
| `tests/acceptance/TC-116/` | 修正、回写、仅报告三种漂移策略与无法表达的差异 |
| `tests/acceptance/TC-123/` | 停止保留卷、入口与数据，启动恢复副本数，入口条件给出已暂停 |
| `tests/acceptance/TC-124/` | 重启重建一次、扩缩容收敛、并发停止串行或 409、四类操作审计可分 |
| `tests/acceptance/TC-125/` | 包默认值物化、未声明标为未限制并给出 LimitRange、覆盖只写提交字段 |
| `tests/acceptance/TC-126/` | 上限小于请求被拒并指出字段，工作负载与投影保持原值 |
| `tests/acceptance/TC-127/` | 停止期间写副本数被接受，启动按最后写入值收敛 |
| `tests/acceptance/TC-72/` | 自身数据库的每日导出与离线恢复 |
| `tests/acceptance/TC-69/` | 应用一致性备份恢复到新实例，四项基线与源应用不变 |
| `tests/acceptance/TC-70/` | 同失败域目标被预检拒绝，不产生成功记录 |
| `tests/acceptance/TC-71/` | 被篡改的演练数据使演练与备份都失败并列出差异 |
| `tests/acceptance/TC-117/` | 登录失败同一结果、按登录名锁定与退出 |
| `tests/acceptance/TC-118/` | 角色变更、改口令、停用与删除前置转移 |
| `tests/acceptance/TC-30/` | 可见性谓词：所有者、列名与未列名成员、访客 |
| `tests/acceptance/TC-31/` | 角色写权限、所有者写定、跨所有者卷引用与策略选择器 |
| `tests/acceptance/TC-32/` | 角色与可见性不改变入口可达性 |
| `tests/acceptance/TC-33/` | 所有者转移：仅全局 Owner，原所有者失去管理权 |
| `tests/acceptance/TC-34/` | 扩展页面写操作按角色与声明授权 |
| `tests/acceptance/TC-36/` | 每类写操作一条审计记录，扩展同流，不可改删 |
| `tests/acceptance/TC-37/` | 敏感值不出投影、日志与自身数据库 |
| `tests/acceptance/TC-79/` | 删除被入口依赖边引用的组件被阻塞并逐个列出依赖者 |
| `tests/acceptance/TC-80/` | 入口证书状态一等项与到期前 14 天告警 |
| `tests/acceptance/TC-1/` | 安装与卸载产品不改变内核 kind 集合；对象类只在其 API 组 |
| `tests/acceptance/TC-2/` | 能力基线不变；扩展能力只在安装期间可读 |
| `tests/acceptance/TC-3/` | 第一方与第三方同类贡献装载、权限判定与卸载一致 |
| `tests/acceptance/TC-4/` | 九类声明逐项可见，组件状态就绪 |
| `tests/acceptance/TC-5/` | 未声明与未安装的贡献不可见，越界写入被拒绝 |
| `tests/acceptance/TC-6/` | 依赖缺失时无贡献生效；一个控制器失败只影响它管理的对象 |
| `tests/acceptance/TC-7/` | 设置项给出当前值、来源与修改入口，写入后当前值变化 |
| `tests/acceptance/TC-8/` | 停用后设置项消失且接口被拒，重新启用后值保留 |
| `tests/acceptance/TC-9/` | 目录条目结构完整，可安装并产生应用与定义；摘要不匹配不可安装 |
| `tests/acceptance/TC-10/` | 来源移除后条目消失、应用保留，升级路径标记来源缺失 |
| `tests/acceptance/TC-14/` | 偏好按账号与扩展分域、跨浏览器一致、不进对象投影 |
| `tests/acceptance/TC-15/` | 偏好的保留语义：声明保留的仍在，默认删除的为空 |
| `tests/acceptance/TC-106/` | 第三方产品以扩展包加入，对象类只在扩展组，页面带声明来源 |
| `tests/acceptance/TC-107/` | 扩展对象引用内核卷时删除受阻并列出引用者 |
| `tests/acceptance/TC-108/` | 扩展能力缺失停在 Pending，安装后继续，自报不构成验证 |
| `tests/acceptance/TC-109/` | 扩展控制器越界写入内核对象与 PVC 被拒，声明范围仍可写 |
| `tests/acceptance/TC-110/` | 停用撤除贡献并停控制器，对象保留，启用后恢复 |
| `tests/acceptance/TC-111/` | 卸载按声明保留或删除数据，保留项进遗留清单；组件、引用、应用与 Provider 依赖逐个阻塞；坏声明只阻塞自身卸载 |
| `tests/acceptance/TC-112/` | 目标框架版本不兼容时安装被拒绝并列出不兼容项 |
| `tests/acceptance/TC-113/` | 升级预检列出框架依赖者，仍有依赖时阻塞 |

## 对象模型

API 组为 `kos.tsic.top`。十类对象：Application、ApplicationDefinition、Volume、Endpoint、Provider、
SystemComponent、BackupPolicy、RestoreJob、NodeDevice、VolumeMigration。其中
ApplicationDefinition、Provider、SystemComponent、NodeDevice 为集群作用域，其余为命名空间作用域。

ApplicationDefinition 与 NodeDevice 没有 status 写者，因此没有 status 字段与 status 子资源；
其余八类带 `status.phase`、`status.health.state` 与 `Reconciled` 条件。

`spec` 字段由契约逐个加入：尚未固定字段的对象保持空 spec，实现不发明线格式。当前已兑现
C-60 的 `spec.owner`（五类带所有者的对象）、C-59 的 `Application.spec.visibility`、
C-41 的 `BackupPolicy.spec.selector`/`schedule`/`consistency`/`target`/`retention`/`drill`、
`status.lastBackup`（基线四项）、`status.lastDrill` 与 `status.running`（在办的长动作），C-42 的
`RestoreJob.spec.policy`/`targetApplication`/`inPlace` 与 `status.phase` 四阶段、`status.backupPath`
（解析出的运行目录）及差异项，C-17 的 `SystemComponent.spec.package`（工件引用与 sha256 摘要）与
C-28 的 `spec.disable`（停用请求）、
C-32 的 `Application.spec.definitionRef`/`namespace`/`config`/`volumes` 与定义侧
`spec.package`/`config`/`template`/`hooks`/`chart`/`requires`、C-66 的
`Application.spec.replicas`/`paused`、C-67 的 `Application.spec.resources`、
`status.resources` 与定义侧 `spec.resources`（包默认值），C-33 的
`status.retryCount`、`status.nextRetryAt`、`status.uninstall`，C-46 的
`Endpoint.status.certificate`（签发者、就绪、到期与告警窗口）与 `CertificateReady` 条件，
以及 C-70 的 `NodeDevice.spec.node`/`collector`/`observedAt`/`devices`。

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
| `POST` | `/api/v1/action-requests` | 应用动作 `retry`/`orphan`/`confirm`/`restart`/`stop`/`start`/`scale`；202 已接受 + 动作记录引用 |

- 读投影每项带 `observedAt` 与 `stale`；陈旧阈值是 `controlplane.StaleThreshold`（2 分钟），所有来源共用。确知发现来源时，投影的 `sources` 列出全部上报的适配器标识。
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
  或升级已回滚的应用，`orphan` 只接受阻塞超过十分钟且阻塞清单非空的 Uninstalling 应用，
  `restart` 只接受已安装（Ready/Upgrading）的应用，`confirm` 只接受外部改动待确认的应用，
  并须给出目标摘要。
- `stop` 写暂停标记且不改 `spec.replicas`；`start` 清除暂停标记；`scale` 写 `spec.replicas`
  （需 `replicas`，非负）。三者以存储对象的 `resourceVersion` 提交，并发写入按 C-05 返回 409
  与当前对象。`restart` 走一次性记录，只重建工作负载，不改用户 spec、卷与入口。
- 四类操作在审计流中各自留有 `stop`、`start`、`restart`、`scale` 记录；审计通过
  `controlplane.Auditor` 注入（C-14）。
- 应用资源规格在写入前校验：数量必须可解析且非负，上限小于请求返回 `invalid_field` 并指出
  `spec.resources.limits.*`；包默认值在创建时物化进 `spec.resources`，更新只覆盖提交的字段。

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

- **删除受阻**（C-17、C-46、C-48）：组件调和者在首次 install 轮给每个受管组件加 finalizer；
  删除仍被入口依赖边引用的组件时对象停在 Terminating，`DeprovisionBlocked` 为真并逐个列出
  依赖者，依赖消失后才释放。前提是门已附加：首次观察之前就删除的组件不被持有（apiserver
  拒绝给删除中的对象新增 finalizer），按卸载退出装载集。依赖边有两条建立路径：入口 spec 的
  `tls.issuerRef`/`auth.providerRef`，以及省略签发者时状态记录的解析结果；入口侧把证书状态
  写成 `status.certificate` 与 `CertificateReady` 条件，投影对到期窗口内的证书给出告警。
- **偏好与退场**：声明了偏好结构的组件在每次装载后把「保留或删除」连同所属组件对象写进
  数据库的域记录；只有组件对象消失（卸载）、按删除门退出装载集，或改为声明另一扩展时，
  未声明保留的域才连同数据删除，声明保留的域留下。包不可读、摘要不匹配、声明非法或依赖
  暂缺的组件对象仍在本轮装载集里，其域保持不动（C-27、C-29）。设置项的值随停用保留，
  不随贡献撤除删除。

停用、卸载、框架版本兼容、其余扩展数据的保留与其余贡献类的语义不在本阶段实现，分别属于
独立契约；组件删除只做依赖检查阻塞，被阻塞期间的数据与工作负载处理不在本阶段。

## 设置、偏好与目录

- **设置域**（C-23）：`pkg/settings` 把已激活组件声明的设置项投影成当前值、来源（声明的写者）、
  修改入口与不可修改原因。写者不是用户或未声明可修改的项没有入口，写入返回 `invalid_field`
  并说明原因；用户写入的项按作用域（集群、节点、用户）取值，节点级项必须带 `node` 且节点名
  须是 DNS 子域，用户级项只读写调用者自己的值。设置项随组件出现与消失，值在停用期间保留。
- **偏好**（C-27）：偏好按扩展标识与账号分域存进控制面数据库，同一账号换浏览器读到同一份；
  未声明的键写入被拒，存储中超出结构的键在读取时被忽略；偏好不是对象，不出现在对象投影里。
- **目录**（C-24）：目录来源声明条目；同步以 `catalogue-sync` 身份写 `ApplicationDefinition`，
  只有定义合法（内容与包摘要一致、依赖满足）的条目才写入。条目带显示信息、版本、依赖、
  安装参数结构与摘要；摘要不匹配或缺依赖的条目仍可见但不可安装并给出原因。条目标识跨来源
  唯一：两个来源声明同一条目标识时，先装载的一方保留该标识，后者记为贡献冲突并降级，同步
  因此不会在轮次间改写同一份集群作用域定义。来源移除后条目消失，已安装应用与定义保留，
  升级路径在目录读取中标为来源缺失。

| 方法 | 路径 | 语义 |
|---|---|---|
| `GET` | `/api/v1/settings` | 设置项投影；`node` 指名节点级项的目标节点 |
| `PUT` | `/api/v1/settings/{id}` | 写一项设置值；只接受用户写者且可修改的项 |
| `GET` | `/api/v1/preferences/{extension}` | 当前账号在该扩展域内的偏好 |
| `PUT` | `/api/v1/preferences/{extension}/{key}` | 写一个已声明的偏好键 |
| `GET` | `/api/v1/catalogue/entries` | 目录条目与是否可安装 |
| `GET` | `/api/v1/catalogue/upgrades` | 已安装应用的升级路径（`Available`/`SourceMissing`） |

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

停止、启动、重启与扩缩容按 C-66 收敛。`stop` 只写 `spec.paused`，调和器把可伸缩工作负载
（Deployment、StatefulSet、ReplicaSet）的副本数收敛到零，卷、PVC、Service 与入口路由保持
不动；暂停期间 health 按有效期望副本零判定（观察到则 Healthy，无法观察则 Unknown），phase
保持进入暂停前的 Ready/Upgrading。`start` 清除暂停标记，副本数回到 `spec.replicas`，未被
写入时回到包渲染的值。`scale` 写 `spec.replicas`，写入值在暂停期间同样被接受，启动后按最后
写入值收敛。`restart` 经一次性动作记录提交，调和器删除渲染过的工作负载并在同一轮按基线重建
一次，不写用户 spec，不删除卷与入口。删除是 KOS 自己的写入，因此它的「缺失」不按外部改动处理：
`reconcile`、`adopt`、`ignore` 三种策略下重启都会重建，且不写 `status.drift`。可伸缩工作负载
之外的对象不参与副本收敛。

基线保存包渲染的清单，同时记下那一轮实际写下的暂停、副本与资源事实。漂移比较前先把基线
按这些事实收敛回来，所以停止与扩缩容不会被读成外部写入：`adopt` 与 `ignore` 策略下两者同样
收敛，漂移记录只针对真正由外部改动的字段。

C-67 的资源规格是 spec 的一部分：`ApplicationDefinition.spec.resources` 声明包默认值，创建
应用时物化进 `spec.resources`，之后定义更新不再影响已存在对象。调和器把 `spec.resources`
写到每个容器与 init 容器的 requests/limits；覆盖只写提交的字段，未覆盖字段保持默认值。未声明
资源的应用不加任何值，`status.resources.unlimited` 为 true，`status.resources.limitRange`
给出仍然生效的命名空间 LimitRange 名称。上限小于请求在写入前被拒绝并指出字段。经原生接口
创建、默认值只落在 `status.resourceDefaults` 的应用同样按冻结默认值判定：API 只提交上限时，
请求取自该快照，上限低于快照请求的写入被拒绝，而写入本身仍只带提交的字段。快照落地之前
（应用仍 Pending、尚未完成首轮调和）判定改用定义当前的默认值，因此这个窗口里上限低于包请求
的写入同样被拒。

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

## 升级、回滚与外部改动仲裁

`pkg/app` 的升级路径按 C-34 的三步执行。应用当前 spec 的摘要与已应用基线不一致时才算升级
请求：把定义指向新包或改配置都算，目录更新定义本身不算，因此定义更新不改变已安装对象
（C-32）。预检先跑：配置差异按字段列出，渲染差异按对象给出创建、更新（含字段路径）与删除，
不可逆项与迁移要求单独成列。基线保存 spec 摘要、包摘要、数据格式、渲染摘要与上一份清单，
所以回滚与应用外写入的判定都不依赖定义对象还在不在。两个同名字段语义不同、不可互相比较：
`status.applied.renderDigest` 是基线保留的**包渲染清单**摘要（不含暂停、副本与资源事实，
这些事实记在旁边），`status.upgrade.renderDigest` 是同一份清单按这些事实收敛之后的**目标**
摘要，也是应用该目标后集群持有的集合与用户确认时提交的摘要。

不可逆项一律拒绝且没有确认绕过：数据格式变更（包声明的 `spec.package.dataFormat` 与基线
不同，删掉声明也算变更）、删除卷声明、缩容卷。规则拒绝时阶段不变，`IrreversibleUpgrade`
条件给出原因，预检结果留在 `status.upgrade`。存储类变化不拒绝，而是在卷的期望策略与实际
策略不一致时给出 `VolumeMigration` 迁移要求。卷的尺寸与策略事实读自真正持有它们的对象：
卷以同名创建的 PVC（声明命名空间内）给出请求尺寸与策略，绑定到的 PV 给出实际策略；绑定了
却读不出尺寸的卷按不可判定处理，预检直接拒绝而不是放行（`VolumeSource` 是读取它们的接缝）。

预检通过后先写 `Upgrading`，下一轮写入新清单并运行包声明的 `upgrade` 钩子；钩子非零退出即
失败，调和器把基线里的上一份清单重新应用，删掉失败升级新建或改名留下的对象，回到 `Ready`
并写入 `UpgradeRolledBack` 事件，集群里只剩上一版本的清单。回滚只保证对象形状，数据由升级前
的备份点负责。同一个目标被回滚后不再自动重试：写新目标
或提交 `retry` 动作才会再次尝试，避免故障包让工作负载来回滚动。

外部写入按 `spec.driftPolicy` 处理：`reconcile`（缺省）以期望值获胜并改写回，`adopt` 把下游值
记进 `Application.status.drift` 且不改用户 spec，`ignore` 只报告差异；后两者保留外部值、写
`Drifted` 失败条件并让健康为 Degraded。只有 `adopt` 记录，并且只有配置结构能表达的差异才记录：
调和器用每个配置字段的探针渲染判断哪些渲染路径由配置驱动，表达不了的差异拒绝记录、漂移
条件保留。声明不可逆升级的应用被外部写入时强制暂停：`Reconciled=False`、原因为外部改动待
确认，期间不写回也不继续升级。用户经控制面提交 `confirm` 动作与目标摘要，API 进程在
`kos-system` 写一条一次性动作记录并带上提交时的 spec 摘要；应用调和器读取后删除。第三项
一致是对下游的观测而不是再渲染一次 spec：调和器按目标清单逐对象读取集群，只有记录目标摘要
既等于该观测、又等于当前 spec 请求的渲染摘要，且记录里的 spec 摘要等于当前 spec 摘要时，才把
它作为新基线恢复调和。目标摘要按当前 spec 的暂停、副本与资源事实收敛后计算，观测侧的清单
走同一份收敛，所以已停止或已扩缩容的应用也能确认。因此没有在外部真正升级过的确认不会被接受，
旧记录消费后也不再生效。

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
- 审计是追加流：每个写操作（含被拒绝的尝试）写一条记录，字段为操作者、对象、动词、结果、时间、来源
  与扩展标识；对象写与账号写（建号、改角色或停用、删除、改口令）同流。集合 apply 按对象取各自的
  判定结果记账，内核对象与扩展对象不会互相带上对方的扩展标识。表由迁移脚本加触发器拒绝改写与删除，
  API 不暴露任何审计写入口。
- 数据库是单文件 SQLite（纯 Go 驱动），存放用户、会话、按扩展分域的偏好、设置值与审计，
  不在调和路径上。schema 由内嵌迁移脚本按版本前滚，`Rollback` 反向执行最新一版。
  数据库不可用时登录、设置与偏好的读写失败（`database_unavailable`），已登录会话的读请求
  从内存副本照常提供，集群与调和不受影响。
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

除登录外，所有路由都要求 `Authorization: Bearer <token>`。`identity.Authorizer` 把 `pkg/access`
的 C-59/C-60 规则接到控制面 API：Guest 只读；Member 只写自己拥有的应用、入口、卷、备份策略、
恢复作业与卷迁移；Owner 写全部。可见性谓词（`Application.spec.visibility`）只过滤面板读取，
不改变入口可达性；`spec.owner` 由 API 以会话身份写入且不可变，只有全局 Owner 能转移；跨所有者
卷引用与备份策略选择器在写入时被拒绝。扩展对象的写角色取声明的 `objectWriters`，未声明时只由
Owner 写。每类写操作同时向 `identity.Audits` 追加一条审计记录（C-14）。

## 构建与测试

```bash
make generate   # 重新生成 deepcopy 与 CRD，并保持与 Go 类型一致
make verify     # 生成物无漂移 + gofmt + go vet
make test       # 单元测试 + 验收测试（自动下载 envtest 二进制）
make acceptance # 只跑 tests/acceptance，输出用例名
```

验收测试使用 envtest 启动真实 kube-apiserver 与 etcd，安装 `config/crd` 后读写对象；
环境变量 `KUBEBUILDER_ASSETS` 指向 envtest 二进制目录，`make test` 会下载并设置。
