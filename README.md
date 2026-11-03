# KOS

KOS 是以 Kubernetes 为运行内核的 homelab 操作系统。集群是原语，单节点是退化形态，不对规模与高可用作承诺；KOS 不替换 Kubernetes，只在显式确认的对象上接管，把它的资源封装成操作系统对象与接口。

本仓是 KOS 的公开入口：三层模型、项目地图、各仓链接与阅读顺序。实现与契约都不在本仓——实现住在各项目的实现仓，契约以设计仓为准。

## 三层模型

| 层 | 承载 | 内容 |
|---|---|---|
| 运行内核 | Kubernetes | 调度、运行时、服务发现、存储绑定与节点代理 |
| 框架层 | kosd | 对象模型、归属与写入、调和、状态与健康、能力契约、控制面 API、扩展接口 |
| 功能包 | 第一方项目与第三方扩展包 | 装应用、管存储、放入口、看设备、备份、升级、管账号等产品面 |

第一方功能包与第三方扩展走同一条扩展通道：以带摘要的包分发，声明贡献、依赖与写入范围，由框架装载，不享有专用通道。功能不需要改框架就能加入，也共用同一套状态、写入者、权限与退场语义。

## 项目地图

18 个项目，各自独立构建、发布与部署。每个项目的设计（范围、契约、验收与决策）住在设计仓 `tsic/kos-design` 的同名目录，实现住在各自的实现仓。

| 项目 | 层 | 交付物 | 职责 | 实现仓 |
|---|---|---|---|---|
| kosd | 平台 | 控制面守护进程 | 对象模型、控制面 API、调和、扩展装载、身份、审计、设置项接口、目录来源、诊断适配器 | <https://github.com/tsic404/kosd> |
| kos-node | 平台 | 每节点代理 | 本节点设备事实与占用（NodeDevice）；不写节点身份与健康 | <https://github.com/tsic404/kos-node> |
| kosadm | 平台 | 管理 CLI | 安装、引导、升级、卸载、救援、状态 | <https://github.com/tsic404/kosadm> |
| kos-sdk | 平台 | 作者 SDK、清单结构与打包工具 | 扩展与应用作者接口 | <https://github.com/tsic404/kos-sdk> |
| kos-conformance | 平台 | 契约测试套件 | 能力、Provider、扩展与应用的一致性 | <https://github.com/tsic404/kos-conformance> |
| kos-dashboard | 桌面 | 浏览器桌面与 BFF | 桌面、窗口、dock、launcher、应用宿主、通知中心、主题、全局搜索 | <https://github.com/tsic404/kos-dashboard> |
| kos-appstore | 系统应用 | 扩展包 | 目录、安装参数、安装与升级入口 | <https://github.com/tsic404/kos-appstore> |
| kos-settings | 系统应用 | 扩展包 | 集群与节点设置、成员与角色、应用可见性、资源占用口径、维护窗口 | <https://github.com/tsic404/kos-settings> |
| kos-taskmanager | 系统应用 | 扩展包 | 概览、应用、资源占用、应用详情 | <https://github.com/tsic404/kos-taskmanager> |
| kos-storage | 系统应用 | 扩展包 | 卷、迁移、使用率与备份状态（只读） | <https://github.com/tsic404/kos-storage> |
| kos-network | 系统应用 | 扩展包 | 入口、证书、端口台账、解析探测 | <https://github.com/tsic404/kos-network> |
| kos-devicemanager | 系统应用 | 扩展包 | 节点硬件、设备、占用与陈旧标记 | <https://github.com/tsic404/kos-devicemanager> |
| kos-filemanager | 系统应用 | 扩展包 | 授权卷内的文件浏览与传输 | <https://github.com/tsic404/kos-filemanager> |
| kos-backup | 系统应用 | 扩展包 | 备份策略、恢复、演练与目标 | <https://github.com/tsic404/kos-backup> |
| kos-updater | 系统应用 | 扩展包 | 版本、组件、兼容性、平台与扩展升级的预检与被阻塞项；应用升级归 kos-appstore | <https://github.com/tsic404/kos-updater> |
| kos-diagnostics | 系统应用 | 扩展包 | 降级原因、证据链与诊断包 | <https://github.com/tsic404/kos-diagnostics> |
| kos-notification | 系统应用 | 扩展包 | 事件与状态变化的渠道投递与记录 | <https://github.com/tsic404/kos-notification> |
| kos-console | 系统应用 | 扩展包 | 原生资源的只读视图 | <https://github.com/tsic404/kos-console> |

生态扩展（虚拟机、共享协议、远程访问、媒体与相册、下载、完整监控与告警、Web 终端等）不是核心交付，暂不建项目目录；它们经同一扩展通道加入，完整清单与不进核心的边界以设计仓 `kosd/01-scope.md` 的 `KOSD-NG-11` 与 `kosd/adr/ADR-034` 为准。

## 仓库

| 仓 | 内容 |
|---|---|
| <https://github.com/tsic404/kos> | 本仓：公开入口，项目地图、各仓链接与阅读顺序 |
| <https://github.com/tsic404/kosd> | 框架层实现仓；KOS 旧实现的全部历史在此 |
| <https://github.com/tsic404/kos-node> 等 17 个仓 | 各项目实现仓，见上表 |
| <https://gitea.tsic.top/tsic/kos-design> | 设计仓：范围、契约、验收与 ADR |

## 实现去向

KOS 的实现（kos-shell）已迁入 <https://github.com/tsic404/kosd>。

本仓不再包含实现代码：没有 Go 模块、构建入口、对象定义与验收用例，也不承载契约正文。要构建、测试或阅读实现，请到对应项目的实现仓；要知道它承诺什么，请到设计仓。

## 公开文档与阅读顺序

- Wiki 入口：<https://github.com/tsic404/kos/wiki>（待建：首个页面需由仓库 owner 在网页创建，创建前该地址回到本仓首页）
- 设计仓：<https://gitea.tsic.top/tsic/kos-design>

阅读顺序：

1. 本文件的三层模型与项目地图。
2. 设计仓根 `README.md`：项目地图、分层与跨项目 AC 归属。
3. 设计仓 `kosd/README.md`、`kosd/01-scope.md` 与 `kosd/02-contracts.md`：框架、内核与跨项目语义。
4. 各项目 `README.md` 与 `01-scope.md`：项目边界、模块与依赖。
5. 各项目 `02-contracts.md` 与 `03-acceptance.md`：对外承诺与判据。
6. ADR：框架与跨项目决策在 `kosd/adr/`，项目内决策在 `<project>/adr/`；编号全局唯一。
