#!/usr/bin/env python3
"""生成 edgeone2api 工具调用验证用的演示数据。"""
import json
import os

BASE = "/tmp/kuku2api_demo"
os.makedirs(BASE, exist_ok=True)

rows = [
    ("华东", "手机", 12999, 12),
    ("华南", "手机", 18999, 15),
    ("华东", "平板", 8999, 8),
    ("华北", "手机", 9999, 10),
    ("西南", "手机", 21999, 9),
    ("华东", "手机", 15999, 14),
    ("华南", "平板", 7499, 11),
    ("华北", "平板", 10999, 6),
    ("西南", "平板", 6999, 7),
    ("华东", "笔记本", 25999, 5),
    ("华南", "笔记本", 28999, 4),
    ("华北", "笔记本", 23999, 3),
    ("西南", "笔记本", 19999, 6),
    ("华东", "智能手表", 3999, 20),
    ("华南", "智能手表", 4599, 18),
    ("华北", "智能手表", 3599, 16),
    ("西南", "智能手表", 4199, 12),
    ("华东", "耳机", 1999, 30),
    ("华南", "耳机", 1499, 25),
    ("华北", "耳机", 1299, 22),
]

with open(os.path.join(BASE, "sales.csv"), "w", encoding="utf-8") as f:
    f.write("region,product,amount,qty\n")
    for r in rows:
        f.write(",".join(str(x) for x in r) + "\n")

total = sum(r[2] for r in rows)
total_qty = sum(r[3] for r in rows)
region_sums = {}
for r in rows:
    region_sums[r[0]] = region_sums.get(r[0], 0) + r[2]
top_region = max(region_sums, key=region_sums.get)

with open(os.path.join(BASE, "expected.json"), "w", encoding="utf-8") as f:
    json.dump({
        "total": total,
        "total_qty": total_qty,
        "order_cnt": len(rows),
        "region_sums": region_sums,
        "top_region": top_region,
        "top_region_amount": region_sums[top_region],
    }, f, ensure_ascii=False, indent=2)

notes = []
notes.append("库存盘点注意事项（用于长上下文 + 工具组合验证）：")
notes.append("仓库 A 存放手机与平板，安全库存阈值：手机 200 台、平板 150 台。")
notes.append("仓库 B 存放笔记本与配件，安全库存阈值：笔记本 80 台。")
notes.append("本季度促销活动 P2026Q4 将于 10 月 10 日开启，涉及手机 8 折、平板 85 折。")
notes.append("促销期间预计日销量翻倍，建议在活动前 3 天完成备货。")
notes.append("仓库 A 当前库存：手机 180 台（低于阈值 200，需补货 20 台以上）、平板 260 台。")
notes.append("仓库 B 当前库存：笔记本 95 台（高于阈值 80）。")
notes.append("物流承运商切换为顺丰特惠，跨省时效 2-3 天。")
notes.append("系统将于 9 月 30 日 02:00-04:00 停机维护，期间订单接口不可用。")
notes.append("客服侧重点：促销价与会员价叠加规则需在 10 月 8 日前完成配置评审。")
notes.append("合规提醒：价格展示需包含划线价与原价对比，避免虚假促销投诉。")
with open(os.path.join(BASE, "inventory_notes.txt"), "w", encoding="utf-8") as f:
    f.write("\n".join(notes))

print(f"sales.csv: {len(rows)} rows")
print(f"total={total} qty={total_qty} top_region={top_region}({region_sums[top_region]})")
print(f"inventory_notes.txt: {len('\n'.join(notes))} chars")
print("written under", BASE)
