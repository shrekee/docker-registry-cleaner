# 二进制发布

每个发布包含以下三个归档和 `SHA256SUMS`：

- `docker-registry-cleaner_<tag>_linux_amd64.tar.gz`
- `docker-registry-cleaner_<tag>_linux_arm64.tar.gz`
- `docker-registry-cleaner_<tag>_darwin_arm64.tar.gz`

归档内含 `registry-cleaner`、该标签源码中的 `README.md` 和 `LICENSE`，以及 `BUILD-INFO.txt`（版本、源码提交、实际 Go 版本和目标平台）。下载全部归档及校验文件后，在同一目录验证：

```sh
sha256sum -c SHA256SUMS       # Linux
# 或：shasum -a 256 -c SHA256SUMS  # macOS
```

校验和能发现文件损坏，但不能单独证明发布者身份；应从可信的 GitHub Release 获取文件和校验和。

## 维护者操作

发布工作流在 Release 发布时运行，也可在 Actions 中手动运行并指定已有已发布 Release 的精确标签。标签采用受限的语义版本格式（允许 `v` 前缀与预发布后缀，不接受 `+` 构建元数据）。构建使用标签解析得到的提交，而不是当前 main；发布自动化则来自受信任的工作流代码，不依赖旧标签中包含脚本。工作流会重新核验标签与 Release，在对应架构的托管 runner 测试打包后的二进制，并验证归档与校验和。当前发布工作流将 Go 工具链固定为 1.27.3（与源码 `go.mod` 的最低版本要求分开）；各归档记录实际 Go 工具链版本。上传使用 GitHub Actions 自带令牌；不需要个人访问令牌。

为已有 `0.1.0` Release 的一次性回填，main 上的 `.github/release-backfill/0.1.0.request` 固定指定标签 `0.1.0` 和提交 `e211d99af23681f4ba4b8953c1a1bc047dacd2c7`。仅对该文件在 main 的 push 触发回填；不移动或重建标签，不会在每次 push 发布。未来版本应使用 Release 发布事件或手动运行，而不是修改此回填请求。

上传不会覆盖已有的不同文件。相同内容可以跳过；若现有资产冲突、Release 不可修改、标签不匹配或校验失败，工作流会失败并保留已有资产供人工核查。上传多个资产不是原子操作：失败可能留下部分资产（包括 GitHub 返回的未完成资产）；先检查 Release 和 Actions 日志，不要盲目删除或覆盖，再决定如何处理。
