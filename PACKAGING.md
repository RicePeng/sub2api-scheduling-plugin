# 构建和安装签名插件

这是 Sub2API 账号调度插件项目（宿主显示名称“账号调用管理”），插件协议 ID 为 `local.cache-rate-manager`。它是第三方插件，不是 Sub2API 官方插件。发布者必须自行保管 Ed25519 私钥；部署者审核并信任公钥。无需也不应关闭宿主签名校验。

## 准备编译器和密钥

从本项目目录执行命令。需要 Go 1.25.0 或更高版本；当前构建使用 Go 1.25.4。相同源码、UI、版本、密钥、目标列表和 Go 工具链生成相同的包字节：构建使用 `CGO_ENABLED=0`、`-trimpath`、`-buildvcs=false`、固定 ZIP 时间、排序文件以及确定性的 Ed25519 签名。升级 Go 工具链会改变二进制和包内容。

建议把密钥放在个人安全目录，备份私钥并限制目录访问权限。**私钥不得进入源码仓库、`ui/`、`dist/`、宿主目录、Sub2API 容器或其运行数据卷。** 工具拒绝在项目或同级 `sub2api` 树内读取或生成签名私钥，也检查路径中的符号链接。其他位置的运行目录由部署者负责避开。Windows 的 POSIX 文件权限不等于 NTFS ACL；请确保密钥目录只对构建账号可读。

```powershell
$go = (Get-Command go -ErrorAction Stop).Source
$keys = Join-Path $env:USERPROFILE ".config\cache-rate-manager-signing"
& $go run ./cmd/packager keygen `
  -private-key (Join-Path $keys "publisher.pem") `
  -public-key (Join-Path $keys "publisher.pub")
```

```sh
go run ./cmd/packager keygen \
  -private-key "$HOME/.config/cache-rate-manager-signing/publisher.pem" \
  -public-key "$HOME/.config/cache-rate-manager-signing/publisher.pub"
```

`keygen` 使用 PKCS#8 PEM 保存私钥，以 Base64 原始 Ed25519 格式保存公钥；两者均以 `O_EXCL` 和权限 `0600` 创建，绝不覆盖已有密钥。工具不打印私钥。不要把私钥作为命令行参数内容或环境变量传递；只传文件路径。重复发布同一发行者版本时复用安全保存的私钥，勿重新生成并误以为仍是原来的信任身份。

## 构建 `.s2plugin`

```powershell
./scripts/build.ps1 `
  -PrivateKey (Join-Path $keys "publisher.pem") `
  -Go $go
```

```sh
./scripts/build.sh -private-key "$HOME/.config/cache-rate-manager-signing/publisher.pem"
```

也可以直接运行完整 CLI；未加入 PATH 的便携 Go 使用 `-go` 指定：

```powershell
& $go run ./cmd/packager build `
  -private-key (Join-Path $keys "publisher.pem") `
  -key-id local-cache-rate-manager-v1 `
  -version 0.1.1 -host-version 0.2.23 `
  -targets linux/amd64,linux/arm64,windows/amd64 `
  -out dist/sub2api-scheduling-plugin-0.1.1.s2plugin `
  -go $go
```

默认交叉编译三个目标。Unix 运行时带执行权限，Windows 运行时以 `.exe` 结尾。所有 `ui/` 常规文件均被收集并声明 SHA-256，符号链接和设备文件被拒绝；请先检查 `ui/` 内没有草稿、凭据或其他不应发布的文件。构建临时目录位于系统临时目录，退出时删除。`-mod=readonly` 不修改 `go.mod` 或 `go.sum`，首次构建仍可能需要下载依赖。

产物包括：

- `dist/sub2api-scheduling-plugin-0.1.1.s2plugin`：ZIP 根目录只有清单和签名，另含 `runtimes/` 与 `ui/` 的已声明文件；不会复制私钥或源码。
- `dist/trusted-publisher.yaml`：只含发行公钥的宿主配置片段，可以交给部署者审核。它在包外，未作为运行时文件打包。

输出文件通过临时文件提交；同名构建产物会被替换，请为需要保留的历史版本选择不同的 `-out`。`manifest.json` 的原始字节由 Ed25519 签名，不应在签名之后修改或重新排版。清单和签名不列入 `manifest.files`，其余每个条目必须有且只有一个哈希声明。

## 信任和安装

1. 通过可靠渠道核对发行公钥，把 `dist/trusted-publisher.yaml` 中的条目合并到实际宿主配置的 `plugins.trusted_publishers`。保留现有其他条目，不要用整个片段覆盖配置文件。按宿主部署方式重启或重新加载配置。
2. 使用宿主插件管理功能上传 `.s2plugin`。签名必须显示为受信任；遇到“不受信任的发布者”时核对 `key_id` 和公钥，不要开启 `allow_unsigned` 绕过。
3. 宿主必须满足 `>=0.2.23 <0.3.0`，协议、传输 API 和 UI Bridge 均为 1，能力为 `openai.oauth.outbound_transport.v1`（`openai` / `oauth`）。推荐版本为 0.2.23。
4. `tested_sub2api_versions` **有意为空数组**。参考宿主 0.2.23 源码并通过本地进程协议测试，不等于已经在真实部署宿主完成安装和运行验证。宿主会提示未验证版本；人工评估后启用时需要明确接受 `accept_untested: true`（UI 中的接受未验证版本确认）。不要改清单伪称已测试。
5. 按项目使用说明配置管理 API 和账号，默认仅监控；先在可控环境核对采集和状态，再决定是否开启明确选择账号的自动管理。启用出站传输绑定会影响相应 OAuth 请求，应按宿主管理流程逐步启用。

私钥始终留在构建机器，宿主只需要 `.s2plugin` 和受信任公钥。轮换密钥时使用新的 `key_id`，先在宿主信任新公钥再发布新签名包；保留旧公钥直到不再需要安装旧版本。

## 检查

```powershell
& $go test ./cmd/packager
```

测试检查精确清单字节验签、声明文件 SHA-256、缺失/重复/未声明路径、Unix 执行权限、可重复 ZIP、UI 入口以及密钥不覆盖和路径边界。它们不替代真实宿主安装验收。
