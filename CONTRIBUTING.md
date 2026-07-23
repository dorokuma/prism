# 贡献指南

## 版本规则

- 格式：`v{major}.{minor}.{patch}`
- **major**：不兼容的 API 变更（当前 0.x 阶段不触发）
- **minor**：新功能（prism setup、provider 路由、模型缓存等）
- **patch**：bug 修复、内部重构、文档更新（不改变对外接口）

每次发版时同时更新：
- README.md 版本号行
- README.md 变更日志
- git tag 推送到远端

## 变更日志

所有改动按时间倒序登记在 README.md 底部「变更日志」节。
格式：`- **日期** — v{版本} — 改动摘要`
