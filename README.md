# Docker Registry Cleaner

一个基于开源 [regclient](https://github.com/regclient/regclient) 的保守型单仓库标签保留工具。**默认仅生成计划，不删除任何内容。** registry 协议、认证和分页由 regclient 处理。

> **重要：删除是破坏性操作。** 请先在一次性测试仓库验证您的 registry、权限和备份恢复方式。执行期间必须冻结仓库写入，包括推送、重新打标签和其他清理任务。检查后到删除之间不存在原子 compare-and-delete；重新校验不能消除竞争条件。本工具不适合在写入仍可能发生的仓库上执行。

## 功能与保留规则

- 对指定的**一个仓库**列出标签及其 manifest digest，输出 JSON 计划；不扫描整个 registry。执行时另输出每次删除结果的 JSON 行。
- 按配置时间从新到旧保留最近的标签数量（`--keep-count`）；保留年龄窗口内的标签（`--keep-age`）；匹配 `--protect` 正则表达式的标签也保留。各项保留规则取**并集**。
- 年龄依据的是 image config 的 `created` 字段，**不是**标签推送时间、仓库更新时间或最后使用时间；重新打标签不会改变它。没有可信时间或创建时间位于未来的条目、镜像索引/多架构列表以及无法识别的元数据须保守保留，不能把“未知”当作旧数据。实际可清理的范围因此可能小于预期。
- 标签与 digest 是不同概念：多个标签可指向同一 digest；索引也可引用子 manifest。计划是**删除标签**，并非证明 digest 或其依赖图不再使用。任何因保留规则而保留的 digest 的其他标签也一起保留；已扫描索引指向的子 manifest 对应标签也保留。
- 仅读取标签列表无法证明完整的 manifest 引用图。工具不直接删除旧 manifest digest 或 blob，也不执行 registry 垃圾回收（GC）。删除标签后，原有无标签 manifest、blob 和存储空间可能仍然存在。不要笼统地对未打标签的 manifest 执行 GC 或删除：它们可能仍是索引的子项或被其他对象引用。有关 GC 请遵循具体 registry 文档，并独立验证完整引用、备份和恢复策略。

## 安装与使用

可从 GitHub Release 下载 Linux amd64、Linux arm64 和 macOS arm64 二进制归档；校验方法及发布流程见 [二进制发布说明](https://github.com/shrekee/docker-registry-cleaner/blob/main/docs/releases.md)。

需要 Go 1.26 或更新版本；依赖版本在 `go.mod`/`go.sum` 中锁定。源码构建：

```sh
go build -o registry-cleaner ./cmd/registry-cleaner
./registry-cleaner --help
```

先生成并人工审查计划（示例仓库及规则仅供说明）：

```sh
./registry-cleaner \
  --repository registry.example.com/team/app \
  --keep-count 10 \
  --keep-age 720h \
  --protect '^latest$' \
  --protect '^release-'
```

`--keep-count` 默认 5，`--keep-age` 默认 `720h`；年龄使用 Go duration，例如 `720h`。`--protect` 是可重复的 Go 正则表达式；`--timeout` 默认 30 分钟。设置规则时请注意未指定的选项仍使用默认值。`--keep-count` 和 `--keep-age` 不能同时为零；保护正则仅是额外保留规则，不能作为唯一保留基线。请根据输出逐项核对目标仓库、标签、digest、保留和候选理由。不得将示例直接用于生产。

### 明确执行及其副作用

只有审查计划、备份、停止所有写入并确认 registry 支持所需操作后，才添加全部三个显式选项：

```sh
./registry-cleaner \
  --repository registry.example.com/team/app \
  --keep-count 10 --keep-age 720h --protect '^latest$' \
  --execute --allow-tag-rewrite --maintenance
```

`--maintenance` 是操作者对维护窗口和写入冻结的声明，**不是锁**。该命令会重新生成和校验当前计划；它不是对之前保存的 JSON 计划的原子应用，执行模式输出新计划后会立即继续，无交互确认；请在执行前先单独运行 dry-run 并核对，期间保持写入冻结。停止写入的整个窗口必须覆盖扫描、校验和删除。每次操作前后都会重列标签并核对全部标签的 digest；即使如此也不是原子操作，也无法验证 registry 未报告的对象。

执行调用固定版本 regclient 的 `TagDelete`。它首先尝试按标签删除；上游实现在该操作不成功时可能尝试回退：创建并推送一个临时空 manifest、把目标标签改指向它，再删除该临时 manifest 的 digest。这会产生额外写入，可能留下临时对象或造成部分完成；`--allow-tag-rewrite` 正是对这种回退及其副作用的明确授权。本工具的写入保护默认仅在原生标签删除得到明确的 HTTP 405 响应时允许回退；对其他失败或不确定的写入结果阻止后续写入并停止。对于**已独立确认是原版 Distribution v2.8.3** 的目标，可额外指定 `--distribution-v2-8-3`，允许该版本的原生标签删除明确返回唯一 `DIGEST_INVALID` 错误时回退。此选项是操作者的版本声明，并不自动识别或验证服务端；请勿用于兼容实现、修改版、代理或其他版本。它不是对原 digest 执行删除，也不会直接清除 blob。不同 registry 对此的支持不同；不要假定在每个 registry 可用。操作账号需要相应权限。

只要发生错误或连接中断，应停止并将操作状态视为**不确定**，先重新盘点核实实际标签状态及可能的临时对象，再决定下一步；不要直接盲目重试。因为没有跨客户端锁、事务或原子条件删除，即使预检通过也不能保证不会与并发写入冲突。

## 安全和局限

- 计划不等同于完整仓库图证明，未扫描其他仓库、引用者、无标签 manifest 或外部依赖。其他客户端或 registry 实现可能具有额外语义。
- 使用最小权限的 registry 账号，保护本机凭据，避免将凭据或敏感的输出提交到版本控制或工单。不要把密码或 token 放在命令行参数中。使用 regclient 的 Docker 凭据配置；有关认证配置，请参阅 regclient 文档并先在测试环境验证。本版本使用系统 CA，不支持 regclient 自定义 CA 或客户端证书选项；所有 registry 请求必须保持原始主机和 scheme；外部 token 服务、跨主机分页或 blob 上传不受支持；需要 POST 刷新 token 的认证流程也不受支持。Docker Hub 等使用外部 token 或重映射主机名的服务可能被拒绝。`--plain-http` 仅供隔离测试环境，不应对真实 registry 或凭据使用。
- 如果读取、分页或必要校验失败，应将计划视为不完整，不执行清理；非零退出不代表已经回滚。
- 此项目不是 registry GC 工具，不保证节省存储空间，也不提供恢复、备份或生产可用性保证。

## 开发与验证

```sh
go test -race ./...
go vet ./...
go build -o registry-cleaner ./cmd/registry-cleaner
```

集成验证使用本地一次性 registry，不使用真实用户仓库或凭据。`integration/run-distribution.sh` 需要自行从官方源码构建 Distribution v3.0.0 和 v2.8.3，并通过 `DISTRIBUTION_V3_BIN` 和 `DISTRIBUTION_V2_BIN` 指定二进制；脚本先构建精确待测产物并打印其 SHA-256。具体已运行的测试和限制以项目发布记录为准。

## 上游与许可

使用 [regclient v0.11.7](https://github.com/regclient/regclient/tree/v0.11.7)，上游采用 Apache License 2.0。项目源码亦采用 [Apache License 2.0](LICENSE)；依赖的各自许可适用。

## English summary

A conservative, single-repository tag-retention tool built on regclient. It defaults to a JSON dry-run plan. Count, image-config-created age, and regex protections form a union; unknown timestamps and indexes are retained. Explicit execution requires all three flags acknowledging execution, a writer-freeze maintenance window, and regclient's possible temporary-manifest tag-rewrite fallback. It does not delete old manifest digests, blobs, or perform GC, and cannot prove a complete registry reference graph. Revalidate in a disposable registry and review the plan before any production use.
