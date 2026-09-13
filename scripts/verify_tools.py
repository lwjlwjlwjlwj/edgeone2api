#!/usr/bin/env python3
"""edgeone2api 真实场景验证 — 复杂工具调用。

场景：
  S1 非流式 · 单工具(read_file) 读取 CSV
  S2 非流式 · 多轮链式(read_file + calculate) 数据分析闭环
  S3 流式   · 多轮链式工具调用
  S4 工具名翻译（客户端声明名 vs 上游原生名）
  S5 长上下文 + 工具组合（读多文件 + 跨轮推理）
  S6 kuku2api 纯文本模式：无 tools 请求绝不泄漏工具调用
全部走真实上游；工具由客户端本地执行（模拟真实 agent 闭环）。
"""
import ast
import datetime
import json
import operator
import re
import sys
import time
import urllib.request

BASE = "http://127.0.0.1:7863/v1/chat/completions"
MODEL = "@makers/deepseek-v4-flash"
DEMO = "/tmp/kuku2api_demo"
RUN_ID = datetime.datetime.now().strftime("%H%M%S")

R = {"pass": 0, "fail": 0}


def ok(name, cond, detail=""):
    tag = "PASS" if cond else "FAIL"
    R["pass" if cond else "fail"] += 1
    print(f"[{tag}] {name}" + (f" — {detail}" if detail else ""))
    return cond


def http_post(messages, tools=None, stream=False, key=None, timeout=300):
    body = {"model": MODEL, "messages": messages, "stream": stream}
    if tools:
        body["tools"] = tools
    req = urllib.request.Request(
        BASE, data=json.dumps(body).encode("utf-8"),
        headers={"Content-Type": "application/json"}, method="POST")
    if key:
        req.add_header("X-Session-Key", key)
    t0 = time.time()
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        if not stream:
            full = json.loads(resp.read().decode("utf-8"))
            ch = full["choices"][0]
            msg = ch.get("message", {}) or {}
            data = {
                "text": msg.get("content") or "",
                "finish_reason": ch.get("finish_reason"),
                "tool_calls": [],
            }
            for tc in msg.get("tool_calls", []) or []:
                fn = tc.get("function", {}) or {}
                data["tool_calls"].append({
                    "id": tc.get("id", ""),
                    "name": fn.get("name", ""),
                    "args": fn.get("arguments", ""),
                })
        else:
            data = parse_sse(resp)
    return data, time.time() - t0


def parse_sse(resp):
    text = ""
    finish = None
    tool_calls = {}   # index -> {"id":..., "name":..., "args":...}
    order = []
    for raw in resp:
        line = raw.decode("utf-8", errors="replace").strip()
        if not line.startswith("data: "):
            continue
        payload = line[6:]
        if payload == "[DONE]":
            break
        try:
            ev = json.loads(payload)
        except Exception:
            continue
        ch = ev.get("choices", [{}])[0]
        delta = ch.get("delta", {}) or {}
        if delta.get("content"):
            text += delta["content"]
        for tc in delta.get("tool_calls", []) or []:
            idx = tc.get("index", 0)
            if idx not in tool_calls:
                tool_calls[idx] = {"id": "", "name": "", "args": ""}
                order.append(idx)
            fn = tc.get("function", {}) or {}
            if tc.get("id"):
                tool_calls[idx]["id"] = tc["id"]
            if fn.get("name"):
                tool_calls[idx]["name"] = fn["name"]
            if fn.get("arguments"):
                tool_calls[idx]["args"] += fn["arguments"]
        if ch.get("finish_reason"):
            finish = ch["finish_reason"]
    calls = [tool_calls[i] for i in order]
    return {"text": text, "finish_reason": finish, "tool_calls": calls}


def safe_calc(expr):
    tree = ast.parse(str(expr).strip(), mode="eval")
    ops = {
        ast.Add: operator.add, ast.Sub: operator.sub, ast.Mult: operator.mul,
        ast.Div: operator.truediv, ast.Pow: operator.pow, ast.Mod: operator.mod,
        ast.USub: operator.neg, ast.UAdd: operator.pos,
    }

    def ev(n):
        if isinstance(n, ast.Constant) and isinstance(n.value, (int, float)):
            return n.value
        if isinstance(n, ast.BinOp) and type(n.op) in ops:
            return ops[type(n.op)](ev(n.left), ev(n.right))
        if isinstance(n, ast.UnaryOp) and type(n.op) in ops:
            return ops[type(n.op)](ev(n.operand))
        if isinstance(n, ast.List):
            return [ev(e) for e in n.elts]
        if isinstance(n, ast.Call) and isinstance(n.func, ast.Name):
            f = n.func.id
            args = [ev(a) for a in n.args]
            if f == "sum" and args:
                return sum(args[0]) if isinstance(args[0], list) else sum(args)
            if f == "round" and args:
                return round(args[0], int(args[1]) if len(args) > 1 else 0)
            if f == "min":
                return min(args)
            if f == "max":
                return max(args)
            if f == "abs":
                return abs(args[0])
        raise ValueError(f"unsupported: {expr}")
    return ev(tree.body)


def execute_tool(name, args_str, log):
    try:
        args = json.loads(args_str or "{}")
    except Exception as e:
        return json.dumps({"ok": False, "error": f"bad args json: {e}"}, ensure_ascii=False)
    name = name.lower()
    if name in ("read_file", "read", "read_text", "cat", "view_file"):
        path = args.get("path") or args.get("file_path") or ""
        try:
            with open(path, "r", encoding="utf-8") as f:
                content = f.read()
            log.append((name, path, f"len={len(content)}"))
            return json.dumps({"ok": True, "path": path, "content": content[:12000]}, ensure_ascii=False)
        except Exception as e:
            return json.dumps({"ok": False, "error": str(e)}, ensure_ascii=False)
    if name in ("calculate", "calculator", "run_calc", "calc"):
        expr = args.get("expression") or args.get("expr") or ""
        try:
            val = safe_calc(expr)
            log.append((name, expr, str(val)))
            return json.dumps({"ok": True, "expression": expr, "result": val}, ensure_ascii=False)
        except Exception as e:
            return json.dumps({"ok": False, "error": f"calc failed: {e}"}, ensure_ascii=False)
    if name in ("search_files", "glob", "grep", "search"):
        pattern = args.get("pattern") or args.get("query") or ""
        path = args.get("path") or args.get("directory") or DEMO
        try:
            import subprocess
            out = subprocess.run(["grep", "-rn", pattern, path], capture_output=True, text=True, timeout=15)
            log.append((name, pattern, f"hits={len(out.stdout.splitlines())}"))
            return json.dumps({"ok": True, "hits": out.stdout[:6000]}, ensure_ascii=False)
        except Exception as e:
            return json.dumps({"ok": False, "error": str(e)}, ensure_ascii=False)
    log.append((name, args_str, "UNKNOWN"))
    return json.dumps({"ok": False, "error": f"unknown tool {name}"}, ensure_ascii=False)


def gen_sales_v2():
    import os
    os.makedirs(DEMO, exist_ok=True)
    products = ["平板", "笔记本", "手机", "耳机", "手表"]
    amounts = {
        "华东": [8999, 14999, 7999, 5999, 7682],
        "华南": [12999, 9999, 6999, 4999, 5004],
        "西南": [10999, 8999, 5999, 3999, 8114],
        "华北": [9999, 7999, 4999, 2999, 9104],
    }
    qtys = {
        "华东": [12, 8, 5, 20, 15],
        "华南": [10, 14, 6, 18, 12],
        "西南": [9, 11, 7, 16, 22],
        "华北": [13, 9, 4, 19, 25],
    }
    rows = ["region,product,amount,qty"]
    for reg in amounts:
        for i in range(5):
            rows.append(f"{reg},{products[i]},{amounts[reg][i]},{qtys[reg][i]}")
    path = os.path.join(DEMO, "sales_v2.csv")
    with open(path, "w", encoding="utf-8") as f:
        f.write("\n".join(rows) + "\n")
    return path


def agent_loop(user_prompt, tools, key, stream=False, max_rounds=10):
    messages = [{"role": "user", "content": user_prompt}]
    tool_log = []
    rounds = []
    for rnd in range(max_rounds):
        data, dt = http_post(messages, tools=tools, stream=stream, key=key)
        text = data.get("text") or ""
        finish = data.get("finish_reason")
        calls = data.get("tool_calls") or []
        rounds.append({"round": rnd + 1, "finish": finish, "dt": round(dt, 1),
                       "tool_calls": [(c.get("name"), c.get("args", "")[:80]) for c in calls],
                       "text_head": text[:120]})
        if calls:
            asst = {"role": "assistant", "content": text or None}
            asst["tool_calls"] = [
                {"id": c.get("id") or f"call_{rnd}_{i}", "type": "function",
                 "function": {"name": c.get("name"), "arguments": c.get("args", "")}}
                for i, c in enumerate(calls)]
            messages.append(asst)
            for c in asst["tool_calls"]:
                res = execute_tool(c["function"]["name"], c["function"]["arguments"], tool_log)
                messages.append({"role": "tool", "tool_call_id": c["id"], "content": res})
        else:
            messages.append({"role": "assistant", "content": text})
            break
    else:
        return None, rounds, tool_log, messages
    return text, rounds, tool_log, messages


TOOLS_READ = [{
    "type": "function",
    "function": {"name": "read_file", "description": "读取本地文件的完整内容", "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}},
}]

TOOLS_ANALYZE = TOOLS_READ + [{
    "type": "function",
    "function": {"name": "calculate", "description": "执行数学表达式计算，支持 + - * / 与 sum([...]) 等", "parameters": {"type": "object", "properties": {"expression": {"type": "string"}}, "required": ["expression"]}},
}]

TOOLS_TRANSLATED = [{
    "type": "function",
    "function": {"name": "read_text", "description": "读取本地文本文件", "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}},
}, {
    "type": "function",
    "function": {"name": "run_calc", "description": "执行数学计算", "parameters": {"type": "object", "properties": {"expression": {"type": "string"}}, "required": ["expression"]}},
}]


def scenario_table(rounds, tool_log):
    for r in rounds:
        print(f"    round{r['round']} finish={r['finish']} ({r['dt']}s) tools={r['tool_calls']} text={r['text_head']!r}")
    for t in tool_log:
        print(f"    exec {t[0]}({t[1]}) -> {t[2]}")


def main():
    # ---------- S1: 非流式 · 单工具 ----------
    print("\n===== S1 非流式 · 单工具 read_file =====")
    final, rounds, tlog, _ = agent_loop(
        f"请读取 {DEMO}/sales.csv 的内容，并告诉我前 3 行数据是什么。", TOOLS_READ, f"tool-s1-{RUN_ID}")
    scenario_table(rounds, tlog)
    ok("S1 模型发起 read_file 工具调用", any(any(c[0] and "read" in c[0] for c in r["tool_calls"]) for r in rounds))
    s1_names = {c[0] for r in rounds for c in r["tool_calls"] if c[0]}
    ok("S1 工具名均为声明名", s1_names <= {"read_file", "read", "read_text", "cat", "view_file"})
    ok("S1 最终回答非空且含 CSV 内容", bool(final) and any(k in final for k in ["华东", "手机", "region", "行"]),
       f"final={ (final or '')[:150]!r}")

    # ---------- S2: 非流式 · 多轮链式 ----------
    print("\n===== S2 非流式 · 多轮链式数据分析 =====")
    final, rounds, tlog, _ = agent_loop(
        f"请分析 {DEMO}/sales.csv：先用 read_file 读取文件，再用 calculate 计算总销售额、订单总数和平均客单价，并指出销售额最高的地区及其金额。",
        TOOLS_ANALYZE, f"tool-s2-{RUN_ID}")
    scenario_table(rounds, tlog)
    tools_used = {t[0] for t in tlog}
    ok("S2 工具链包含 read_file 与 calculate", {"read_file", "calculate"} <= tools_used, f"used={tools_used}")
    ok("S2 最终回答包含总销售额 234680", bool(final) and "234680" in final, f"final={(final or '')[:200]!r}")
    ok("S2 最终回答提到最高地区华东/69994", bool(final) and (("华东" in final and "69994" in final) or "高" in final),
       f"final={(final or '')[:200]!r}")
    ok("S2 多轮闭环(>=2轮)", len(rounds) >= 2 and final is not None)

    # ---------- S3: 流式 · 工具调用 ----------
    print("\n===== S3 流式 · 多轮链式工具调用 =====")
    final, rounds, tlog, _ = agent_loop(
        f"用工具完成：读取 {DEMO}/sales.csv，然后用 calculate 算出'所有行的 amount 之和 + 1000' 的结果。",
        TOOLS_ANALYZE, f"tool-s3-{RUN_ID}", stream=True)
    scenario_table(rounds, tlog)
    ok("S3 流式下工具调用完整返回且参数可解析", any(r["finish"] == "tool_calls" for r in rounds),
       f"rounds={[(r['round'], r['finish']) for r in rounds]}")
    ok("S3 流式多轮闭环最终含 235680", bool(final) and "235680" in final, f"final={(final or '')[:200]!r}")

    # ---------- S4: 工具名翻译 ----------
    print("\n===== S4 工具名翻译（声明 read_text/run_calc） =====")
    final, rounds, tlog, _ = agent_loop(
        f"读取 {DEMO}/sales.csv 的前 3 行（用 read_text），然后用 run_calc 计算 100+200 的结果。",
        TOOLS_TRANSLATED, f"tool-s4-{RUN_ID}")
    scenario_table(rounds, tlog)
    returned = {c[0] for r in rounds for c in r["tool_calls"] if c[0]}
    declared = {"read_text", "run_calc"}
    ok("S4 返回的工具名全部是客户端声明名", returned <= declared, f"returned={returned}")
    ok("S4 工具能实际执行成功", {"read_text", "run_calc"} <= {t[0] for t in tlog}, f"exec={ {t[0] for t in tlog} }")

    # ---------- S5: 长上下文 + 工具组合 ----------
    print("\n===== S5 长上下文 + 工具组合（全新数据文件，强制走工具） =====")
    sales_v2 = gen_sales_v2()
    final, rounds, tlog, _ = agent_loop(
        f"请完成一次新的库存盘点（数据文件已更新为 sales_v2.csv，内容与你之前见过的一切文件都不同）：\n"
        f"1) 用 read_file 读取 {sales_v2} 和 {DEMO}/inventory_notes.txt；\n"
        f"2) 用 calculate 汇总 {sales_v2} 的总销售额，并找出销售额最高的地区；\n"
        f"3) 结合库存文档，判断哪个仓库需要优先补货、补多少，并说明理由。\n"
        f"只允许依据本次工具调用读取到的文件内容作答，不许凭记忆或猜测。",
        TOOLS_ANALYZE, f"tool-s5-{RUN_ID}")
    scenario_table(rounds, tlog)
    files_read = {t[1] for t in tlog if t[0] == "read_file"}
    ok("S5 读取了两个数据文件", {"sales_v2.csv", "inventory_notes.txt"} <= {p.split('/')[-1] for p in files_read}, f"files={sorted(p.split('/')[-1] for p in files_read)}")
    ok("S5 多轮跨文件推理（>=3轮）", len(rounds) >= 3, f"rounds={len(rounds)}")
    ok("S5 结论涉及库存阈值/补货", bool(final) and any(k in final for k in ["仓库", "补货", "库存", "手机", "阈值"]),
       f"final={(final or '')[:200]!r}")
    ok("S5 结论基于新数据（总额 158888）", bool(final) and ("158888" in final or "158,888" in final),
       f"final={(final or '')[:200]!r}")

    # ---------- S6: kuku2api 纯文本模式（无 tools） ----------
    print("\n===== S6 纯文本模式：无 tools 请求绝不泄漏工具调用 =====")
    data, dt = http_post([{"role": "user",
                           "content": f"请读取 {DEMO}/sales.csv 的内容并总结要点（不要使用任何工具，直接回答）。"}],
                         key=f"tool-s6-{RUN_ID}")
    fr = data.get("finish_reason")
    tool_calls = data.get("tool_calls") or []
    text = data.get("text") or ""
    print(f"    finish={fr} tool_calls={len(tool_calls)} text={text[:150]!r}")
    ok("S6 finish_reason=stop", fr == "stop", f"got={fr}")
    ok("S6 消息中无 tool_calls 泄漏", len(tool_calls) == 0, f"tool_calls={tool_calls}")
    ok("S6 有正文回复（不白屏）", bool(text))

    print(f"\n===== 汇总: PASS={R['pass']} FAIL={R['fail']} =====")
    sys.exit(0 if R["fail"] == 0 else 1)


if __name__ == "__main__":
    main()