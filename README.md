# KOS

KOS 是运行在 Kubernetes 上的 homelab 操作系统。本仓是 **kos-shell** 的实现：内核对象模型、
归属标记、扩展接口与 ABI 的实现骨架。设计仓（意图与承诺）为
<https://gitea.tsic.top/tsic/kos-design>，契约以 `docs/02-contracts.md` 为准。

当前阶段只交付内核对象模型与 ABI，不含调和循环、控制面 HTTP 层与扩展装载。

## 布局

| 路径 | 内容 |
|---|---|
| `api/` | KOS API 组标识、十类内核对象的封闭集合、双版本 scheme |
| `api/v1alpha1/` | 第一版对象类型与状态形状 |
| `api/v1beta1/` | 第二版对象类型；与 v1alpha1 同结构，是存储版本 |
| `config/crd/` | 由 Go 类型生成的 CRD 清单（`make generate`） |
| `pkg/markers/` | C-02 归属标记的键、载体与校验 |
| `pkg/abi/` | ABI 自检：kind 集合、作用域、双版本同结构、无 webhook |
| `tests/acceptance/TC-104/` | 同一对象双版本读取的字段一致性 |
| `tests/acceptance/TC-105/` | 转换策略为无转换、集群无准入 webhook |

## 对象模型

API 组为 `kos.tsic.top`。十类对象：Application、ApplicationDefinition、Volume、Endpoint、Provider、
SystemComponent、BackupPolicy、RestoreJob、NodeDevice、VolumeMigration。其中
ApplicationDefinition、Provider、SystemComponent、NodeDevice 为集群作用域，其余为命名空间作用域。

ApplicationDefinition 与 NodeDevice 没有 status 写者，因此没有 status 字段与 status 子资源；
其余八类带 `status.phase`、`status.health.state` 与 `Reconciled` 条件。

`spec` 字段由后续契约逐个加入。本阶段每类对象的 spec 为空结构：契约尚未固定字段时，
实现不发明线格式。

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

## 构建与测试

```bash
make generate   # 重新生成 deepcopy 与 CRD，并保持与 Go 类型一致
make verify     # 生成物无漂移 + gofmt + go vet
make test       # 单元测试 + 验收测试（自动下载 envtest 二进制）
make acceptance # 只跑 TC-104 / TC-105，输出用例名
```

验收测试使用 envtest 启动真实 kube-apiserver 与 etcd，安装 `config/crd` 后读写对象；
环境变量 `KUBEBUILDER_ASSETS` 指向 envtest 二进制目录，`make test` 会下载并设置。
