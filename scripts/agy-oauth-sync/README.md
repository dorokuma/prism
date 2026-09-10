# Gemini OAuth dual-write：agy token 目录权限适配

prism 刷新 Google / Antigravity token 时以

`/root/.gemini/antigravity-cli/antigravity-oauth-token`

为 refresh_token 权威源，成功后整包回写该文件（保留 `auth_method` / `id_token`，写入**真实** expiry，不吃 RefreshSkew）。
prism 服务以 `User=prism` 运行，且 unit 开了 `ProtectHome=true`，默认读不到 `/root`。
agy 每次刷新还会把该文件重写成 `0600 root:root`，再次把 prism 锁在门外。

本目录交付可安装的 unit 片段，**不**由 `deploy.sh` 自动执行，也不改 `/etc/credstore`。

## 为什么绑父目录而不是单个文件 / 拷进 /var/lib/prism

选 **ReadWritePaths 绑父目录** `/root/.gemini/antigravity-cli`，是为了在 `ProtectHome=true` 下让 prism 看见并回写 token 文件；**不是**为了 tmp+rename。

写路径事实（本机实测形态）：

- 目录 mode 是 `755 root:root`。`User=prism` 在该目录 `os.CreateTemp` **必 EACCES**。
- `StoreAgyToken` 先试同目录 tmp+rename，失败后回退 **in-place truncate + fsync + Close**。生产里这条回退是常态，不是例外。
- 因此绑父目录**买不到**原子 rename；绑单个 token 文件同样只能 truncate 那个 inode。选目录绑定只是少一层 path-unit 拷贝、没有“agy 刚写完 prism 还读到旧拷贝”的窗口。RT 不轮换，权威源就是 agy 文件本身。

## 暴露面（完整性 + 提权桥，不只是 conversations 隐私）

父目录绑定之后，同级内容对 `User=prism` 可见。本机实际存在、且与 token 同级的敏感项包括：

| 路径 | 本机权限 | 风险 |
| --- | --- | --- |
| `bin/`（含 `agy` 可执行文件） | 目录 `755 root:root`，`agy` 为 root 可执行 | prism 被攻陷后若能篡改，root 随后执行该二进制 → **完整性破坏 + 提权桥** |
| `updater/` | `755 root:root`（内部 `update_status.json` 等 `0600 root`） | 更新状态/锁文件，同属 root 后续执行链 |
| `settings.json` | `0600 root` | agy 配置 |
| `history.jsonl` | `0600 root` | 命令/会话历史 |
| `conversations/` | 目录 `755`，库文件多为 `644` | 会话隐私（旧文档只写了这一项，不完整） |

`prism-agy-oauth.conf` 用 `InaccessiblePaths=` 覆盖上表 `bin/`、`updater/`、`settings.json`、`history.jsonl`（写成上述实际存在的绝对路径）。**部署前必须**用下面之一确认 `InaccessiblePaths` 与 `ReadWritePaths` 叠加后子路径真的不可及、且父目录 token 仍可写——优先级随 systemd 版本可能不同，不要只看手册：

```bash
systemd-analyze verify /etc/systemd/system/prism.service
# 或装上 drop-in 后起一个测试 unit，以 User=prism 探测上述路径是否 EACCES，
# 同时确认 antigravity-oauth-token 仍可打开读写。
```

若不能接受剩余暴露（conversations 等仍可见），改成把 token 拷进 `/var/lib/prism` 再设 agyPath，不要绑 `/root/.gemini`。

## 做什么

1. systemd drop-in：给 `prism.service` 增加对该 **目录** 的 `ReadWritePaths`，并用 `InaccessiblePaths` 挡住 `bin/`、`updater/`、`settings.json`、`history.jsonl`。
2. path unit：监视 token 文件变化，变化后 `chown root:prism` + `chmod 0660`，让 prism 能就地回写。
3. prism 侧：agy 文件不可写或 JSON 损坏时只记日志、仍写自己的 `/var/lib/prism/oauth/Gemini.json`，不会把刷新打成致命失败。invalid_grant 走退避软闩，不再写永久 `.invalid`。

## 安装（人工，本仓库 worker 不会执行）

在仓库根：

```bash
sudo bash scripts/agy-oauth-sync/install.sh
```

脚本要求 token 文件已经存在（agy 先登录），只把文件拷到 `/etc/systemd/system/`，**不** `daemon-reload`、**不** enable、**不** restart。
拷完后由操作者执行：

```bash
sudo systemd-analyze verify /etc/systemd/system/prism.service   # 确认 InaccessiblePaths 叠加
sudo systemctl daemon-reload
sudo systemctl enable --now agy-oauth-token.path
sudo systemctl start agy-oauth-token-acl.service   # 立刻打一次 ACL，不必等 agy 下次写文件
sudo systemctl restart prism                       # 加载 drop-in 的 ReadWritePaths / InaccessiblePaths
```

## 卸载

```bash
sudo systemctl disable --now agy-oauth-token.path
sudo rm -f /etc/systemd/system/agy-oauth-token.path \
           /etc/systemd/system/agy-oauth-token-acl.service \
           /etc/systemd/system/prism.service.d/agy-oauth.conf
sudo systemctl daemon-reload
sudo systemctl restart prism
```

## 文件

| 文件 | 安装目标 |
| --- | --- |
| `prism-agy-oauth.conf` | `/etc/systemd/system/prism.service.d/agy-oauth.conf` |
| `agy-oauth-token.path` | `/etc/systemd/system/agy-oauth-token.path` |
| `agy-oauth-token-acl.service` | `/etc/systemd/system/agy-oauth-token-acl.service` |
