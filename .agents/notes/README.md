# 架构决策与踩坑笔记规范

本目录记录仓库演进过程中的关键架构决策、技术权衡及踩坑经验。
参考模板：[_template.md](_template.md)

## 何时必须写笔记（满足任一即须写笔记）
触发优先于豁免：单文件 workaround、降级、特判必须记。
1. SQLite schema、索引格式、配置文件格式、对外 API 契约变更
2. 跨两个以上模块或跨仓库
3. 否决了看似更好的方案
4. 临时降级或 workaround 或特判
5. 与 upstream 的故意分歧
6. 性能取舍参数的取值原因

## 何时不用写
- 版本号 bump
- 文案样式
- 行为不变的单文件 bug 修复
- 依赖小版本升级
- 纯补测试

## 命名规则
- 格式：`YYYYMMDD-slug.md`（例如 `20260916-token-usage-sqlite.md`）
- 参考模板：[_template.md](_template.md)

## 维护规矩
- **旧笔记只加 superseded 链接、不改写旧结论**：方案更新时，旧笔记仅在 frontmatter 中更新 `status: superseded` 与 `superseded_by`，并在新笔记中填写 `supersedes`。严禁修改旧笔记的历史事实与旧结论。
