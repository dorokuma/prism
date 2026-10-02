---
status: superseded
superseded_by: "20260929-clinepass-multi-account-metapi.md"
supersedes: ""
模块: planusage, cmd/prism
---

# Legacy Test Failure: TestRunUsageMergesAgyGemini

## 描述
`cmd/prism` 包中的 `TestRunUsageMergesAgyGemini` 在改动前后均失败，与本次 ClinePass 多账号改动无关。

## 失败条件
该测试验证 `prism usage --json` 能否把 usage DB（5 条）与 agy index（1 条）合并为 6 条请求。实际返回 5 条，说明 agy 索引的 1 条请求未被合并逻辑计入。

## 对门禁的影响
- `go test ./...` 整体 FAIL
- 但 `internal/planusage` 包全绿，`cmd/prism` 中本次改动的相关测试全部通过
- 若 CI 要求全绿，需单独修复此测试

## 建议
后续跟进，不在本次范围。
