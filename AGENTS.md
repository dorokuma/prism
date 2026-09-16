# Agent 协作指南

## 铁律
1. **构建与测试**：构建命令 `go build -o prism ./cmd/prism`；改动必须通过 `go test ./...`、`scripts/test_*.sh` 与 `python3 scripts/test_generate_mcp_tools.py`。
2. **禁止提交密钥与配置**：严禁提交真实 `config.yaml`、`.env` 或包含 `sk-*`/Token 的密钥；生产配置位于 `/var/lib/prism/`。
3. **架构分层约束**：业务入口统一收敛在 `cmd/prism`，核心实现位于 `internal/`，禁止向外部暴露未封装内部包。
4. **决策与设计变更必记**：涉及 SQLite schema、索引格式、配置文件格式、API 契约变更；跨两个以上模块或跨仓库；否决看似更优的方案；临时降级、workaround、特判；与 upstream 的故意分歧；性能取值原因，必须按规范在 `.agents/notes/` 记录。

## 索引
- 现状与接口文档：[README.md](README.md)
- 版本与发布规范：[CONTRIBUTING.md](CONTRIBUTING.md)
- 架构决策与踩坑：[.agents/notes/](.agents/notes/)

## 关联仓库
- ctxmode 与 codegraph-go（同为 MCP 工具链组件）
