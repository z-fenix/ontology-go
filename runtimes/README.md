# 函数运行时（TypeScript / Python）

动作类型可以用 `function: { runtime: go | typescript | python, ref: <名称> }` 实现。Go 函数编译进 Action Service；TypeScript 和 Python 函数跑在独立的运行时服务里，Action Service 通过 HTTP 调用。

| 运行时 | 目录 | 依赖 | 启动示例 |
|---|---|---|---|
| Python | `runtimes/python` | 仅标准库，Python 3.9+ | `python3 runtimes/python/example_functions.py`（端口 9100） |
| TypeScript | `runtimes/typescript` | 无，Node 22.6+ | `node --experimental-strip-types runtimes/typescript/example_functions.ts`（端口 9200） |

## Action Service 配置

| 环境变量 | 说明 |
|---|---|
| `ONTO_FUNCTION_RUNTIMES` | `python=http://localhost:9100,typescript=http://localhost:9200` |
| `ONTO_FUNCTION_CALLBACK_URL` | Action Service 在运行时看来的地址，如 `http://action-service:8081`；配置了运行时就必须设 |
| `ONTO_FUNCTION_RUNTIME_TOKEN` | 可选；设了之后 Action Service 调用运行时会带 `Authorization: Bearer <token>`，运行时用同名环境变量校验 |
| `ONTO_FUNCTION_TIMEOUT` | 单次调用超时，默认 `10s` |

## 调用协议

Action Service 对每次调用发 `POST <运行时>/functions/<ref>`：

```json
{
  "ref": "groundAircraft",
  "actor": { "id": "alice", "kind": "USER" },
  "params": { "reason": "bird strike" },
  "objects": { "aircraft": { "type": "Aircraft", "pk": "B-1234", "version": 2, "props": { "status": "ACTIVE" } } },
  "lists": {},
  "reader": { "url": "http://action-service:8081/internal/function-reads/<一次性令牌>" }
}
```

- 返回 `200 {"edits": [...]}`。每条编辑是 `{op, type, pk, changes?, expectedVersion?, linkType?, target?}`，`op` 取 CREATE / MODIFY / DELETE / LINK / UNLINK。
- 业务规则拒绝返回 `422 {"errorCode": "...", "message": "..."}`，错误码原样返回给调用者。
- 其他非 200 状态都按 `FUNCTION_FAILED` 处理。

函数需要读更多对象时，调用 `reader.url` 下的两个接口：
- `POST /get`，请求体 `{type, pk}`。
- `POST /find`，请求体 `{type, eq?, in?, limit?}`。

这两个接口和 Go 函数走同样的规则：按调用者做可见性检查（看不到的对象等同于不存在），共用每次调用 10 万个对象的读取上限。读回调的令牌在调用结束后立即失效。

**运行时只计算编辑，不直接写库。** 返回的编辑由 Action Service 做类型转换和约束校验，再和规则产生的编辑一样，在一个事务里提交。

## 安全

- `/internal/function-reads/` 路径只靠随机令牌保护，只应暴露在运行时所在的内部网络。
- 运行时服务本身也不应对外暴露；建议设置 `ONTO_FUNCTION_RUNTIME_TOKEN`。

## 测试

```
python3 runtimes/python/test_runtime.py
node --experimental-strip-types --test runtimes/typescript/runtime.test.ts
```
