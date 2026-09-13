#!/usr/bin/env python3
"""edgeone2api 真实场景验证 — 长上下文 + 多轮对话连续性。

流程：
  1. 首次请求携带一份长文档，要求总结（X-Session-Key 绑定会话）
  2. 第二问：追问文档中的具体版本号与修复项
  3. 第三问：追问更深处细节（升级步骤）
全部通过真实上游完成，非 mock。
"""
import json
import sys
import time
import urllib.request

BASE = "http://127.0.0.1:7863/v1/chat/completions"
MODEL = "@makers/deepseek-v4-flash"
KEY = "v1-longctx-003"


def build_doc() -> str:
    paras = []
    paras.append("【天穹数据平台 v1.4 版本发布说明】")
    paras.append(
        "天穹数据平台是由星环实验室自主研发的一站式数据治理与智能分析平台，本次 v1.4 "
        "版本围绕稳定性、查询性能与安全合规三个方向进行了全面升级，共修复 27 个缺陷，"
        "新增 5 项能力，并调整了 3 项 API 的默认行为。"
    )
    paras.append(
        "一、核心新能力。1) 新增列级血缘追踪（Lineage Explorer），支持对 200 万级表列的"
        "上下游依赖进行秒级回溯；2) 智能物化视图推荐引擎（MV Advisor）上线，可根据查询模式"
        "自动推荐并预计算物化视图，实测将三类典型聚合查询的 P95 延迟从 3.2 秒降至 0.4 秒；"
        "3) 数据质量规则模板库扩充至 120 个开箱即用模板，覆盖空值率、唯一性、取值范围、"
        "跨表一致性四类场景。"
    )
    paras.append(
        "二、关键修复。本次修复的三个最关键问题：其一是分布式查询引擎在极端偏斜键（单键"
        "占比超过 40%）下的 OOM 崩溃（缺陷编号 DB-2041）；其二是数据同步任务在目标端为"
        "PostgreSQL 15 时偶发的序列（sequence）不一致问题（DB-2107）；其三是管理控制台的"
        "审计日志在导出 CSV 超过 10 万行时出现乱码与截断（DB-2133）。"
    )
    paras.append(
        "三、升级步骤。升级共分四步：第一步，备份元数据库并导出当前配置快照（config "
        "snapshot），建议保留最近 7 个快照；第二步，滚动升级控制面节点（先升级 coordinator "
        "再升级 worker，最小停机窗口为 30 秒）；第三步，执行一次性数据迁移脚本 "
        "migrate_v13_to_v14.py，该脚本会将旧版列统计信息重建为新的 Histogram 格式；第四步，"
        "在灰度环境验证 24 小时后，将生产流量逐步切至新版本，并观察 15 分钟内的慢查询指标。"
    )
    paras.append(
        "四、行为变更。1) /api/v1/query 接口的默认超时从 60 秒调整为 120 秒；2) 建表语句中"
        "未指定分区键时将不再自动创建默认分区，而会返回显式告警；3) 旧版 JDBC 驱动（低于 "
        "4.2.0）将无法连接 v1.4 集群。以上变更均可在兼容模式（compat=true）下临时关闭。"
    )
    paras.append(
        "五、性能基准。在 128 核 × 1TB 内存的标准测试集群上，TPC-DS 100TB 规模下，v1.4 "
        "相比 v1.3 整体查询耗时下降 31%；小文件合并（compaction）吞吐提升 2.4 倍；并发会话"
        "数上限从 512 提升至 2048。"
    )
    paras.append(
        "六、安全合规。新增细粒度数据脱敏策略引擎，支持按字段、按用户组、按会话级动态脱敏；"
        "审计日志新增操作指纹（fingerprint）字段，便于安全团队做异常行为聚类；通过等保三级"
        "与 ISO 27001 复评。"
    )
    paras.append(
        "七、已知问题。1) 物化视图在并发刷新冲突时可能触发额外的一次全量重建，团队预计在 "
        "v1.4.1 修复；2) Lineage Explorer 对存储在对象存储（S3/OSS）中的外部表暂不支持"
        "跨桶血缘。建议用户在本版本中优先使用托管表。"
    )
    paras.append(
        "八、完整缺陷修复清单（本版本 27 项中的代表性条目）。DB-2041 极端偏斜键 OOM；"
        "DB-2107 PostgreSQL 15 序列不一致；DB-2133 审计日志 CSV 导出乱码；DB-2140 物化视图"
        "增量刷新在晚到数据场景下的丢失行问题；DB-2152 窗口函数 OVER (ORDER BY) 在分区裁剪"
        "后的错误排序；DB-2161 Ranger 策略缓存未在 60 秒内失效导致越权风险；DB-2170 慢查询"
        "日志在写入对象存储时偶发重复记录；DB-2183 千字段大宽表建表耗时从 8 秒劣化为 42 秒；"
        "DB-2195 流式写入 checkpoint 回放时偶现的重复消费；DB-2202 JDBC 批量插入空字符串被"
        "误判为 NULL；DB-2210 OFFSET 分页在并行扫描下偶发跳行；DB-2217 目录服务在 LDAP 主备"
        "切换期间 30 秒不可用；DB-2224 任务调度器在闰年 2 月 29 日触发错误的日级分区清理。"
    )
    paras.append(
        "九、核心表数据字典（节选）。dwd_order：订单明细宽表，包含 order_id（订单号，"
        "主键）、buyer_id（买家 ID）、seller_id（卖家 ID）、order_amount_net（净额口径，"
        "已扣除优惠与退款）、order_amount_gross（毛额口径）、pay_channel（支付渠道，"
        "取值 alipay/wechat/card/cash）、order_status（状态机：CREATED/PAID/SHIPPED/"
        "FINISHED/CLOSED）、region_code（地域码，关联 dim_region）、created_at（下单时间）。"
        "dim_user：用户维度表，含 user_level（L1-L5 五档）、reg_source（注册渠道）、"
        "first_order_ts（首单时间）。ads_sales_daily：销售日汇总表，按 region_code + "
        "date_key 粒度聚合 GMV、订单数、客单价三个指标。"
    )
    paras.append(
        "十、附录：API 兼容矩阵。旧接口 /api/v1/query 在 compat=true 下保持 60 秒超时；"
        "新接口 /api/v1/query/v2 默认 120 秒超时并返回列级统计直方图。数据迁移脚本执行时长"
        "与表数量线性相关，实测 5000 张表约需 25 分钟；期间控制面只读，写入请求返回 503。"
        "运维团队建议在业务低峰期执行，并提前在监控大盘开启迁移进度看板（dashboard id: "
        "migration-v14-progress）。"
    )
    paras.append("十一、完整数据字典（自动化生成，共 60 个字段，节选关键字段）。")
    field_defs = [
        ("dim_region", "region_key", "地域主键，遵循 GB/T 2260 行政区划编码"),
        ("dim_region", "region_level", "层级：1=省 2=市 3=区县"),
        ("dim_date", "date_key", "日期主键，格式 YYYYMMDD"),
        ("dim_date", "is_holiday", "是否法定节假日（0/1）"),
        ("dim_sku", "sku_id", "商品 SKU 主键"),
        ("dim_sku", "category_path", "类目路径，如 家电/大家电/冰箱"),
        ("dim_sku", "brand_name", "品牌名"),
        ("dwd_order", "order_amount_net", "净额口径，已扣除优惠与退款"),
        ("dwd_order", "order_amount_gross", "毛额口径"),
        ("dwd_order", "refund_amount", "退款金额，售后闭环回写"),
        ("dwd_order", "coupon_discount", "优惠券抵扣金额"),
        ("dwd_order", "freight_amount", "运费金额"),
        ("dwd_order", "packet_id", "物流包裹号，关联 dwd_logistics"),
        ("dwd_order", "warehouse_code", "履约仓库编码"),
        ("dwd_logistics", "logistics_status", "物流状态：IN_TRANSIT/DELIVERED/RETURNED"),
        ("dwd_logistics", "first_scan_ts", "首次揽收时间"),
        ("ads_sales_daily", "gmv_net", "净额口径 GMV"),
        ("ads_sales_daily", "gmv_gross", "毛额口径 GMV"),
        ("ads_sales_daily", "order_cnt", "订单数"),
        ("ads_sales_daily", "aov", "客单价 = gmv_net / order_cnt"),
    ]
    for i in range(60):
        if i < len(field_defs):
            t, f, d = field_defs[i]
            paras.append(f"- {t}.{f}：{d}（字段序号 {i}）")
        else:
            paras.append(f"- ext_field_{i:03d}：自动化扩展字段，类型 STRING，默认 'N/A'（字段序号 {i}）")
    paras.append("十二、完整缺陷清单（27 项自动化展开）。")
    extra_defects = [
        ("DB-2231", "物化视图基表 DDL 变更后元数据未自动失效"),
        ("DB-2238", "UDF 在并发注册时偶发 ClassNotFound"),
        ("DB-2244", "跨集群复制任务在断网重连后增量位点回退 5 分钟"),
        ("DB-2250", "bitmap 索引在更新密集表上膨胀未自动合并"),
        ("DB-2257", "查询计划缓存对带字面量的谓词复用导致计划陈旧"),
        ("DB-2263", "parquet 谓词下推在嵌套 struct 上失效"),
        ("DB-2269", "扩容节点后数据再平衡任务抢占写带宽"),
        ("DB-2275", "审计日志按天滚动时 23:59 至 00:01 的窗口重叠"),
        ("DB-2281", "JDBC 预编译语句在分片键推断错误时全表扫描"),
        ("DB-2287", "轻量级物化视图在 REPLACE TABLE 后丢失刷新依赖"),
    ]
    for code, desc in extra_defects:
        paras.append(f"- {code}：{desc}")
    for i in range(27 - len(extra_defects)):
        paras.append(f"- DB-23{40+i}：自动化生成缺陷条目，描述为回归测试覆盖场景 {i}（占位）")
    return "\n\n".join(paras)


def chat(messages, key=None, stream=False, timeout=180):
    body = {
        "model": MODEL,
        "messages": messages,
        "stream": stream,
    }
    req = urllib.request.Request(
        BASE,
        data=json.dumps(body).encode("utf-8"),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    if key:
        req.add_header("X-Session-Key", key)
    t0 = time.time()
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        data = json.loads(resp.read().decode("utf-8"))
    dt = time.time() - t0
    return data, dt


def main():
    doc = build_doc()
    print(f"[doc] 长文档字符数: {len(doc)}")

    # 第 1 问：总结（携带长文档）
    r, dt = chat([{"role": "user", "content": doc + "\n\n请用不超过 200 字总结这份文档的核心要点。"}], key=KEY)
    c1 = r["choices"][0]["message"]["content"]
    print(f"\n[Q1 总结] {dt:.1f}s finish={r['choices'][0]['finish_reason']}\n{c1}")

    # 第 2 问：只发新消息，依赖会话记忆
    r, dt = chat([{"role": "user", "content": "这份文档的新版本号是多少？它修复的三个最关键问题分别是什么？请逐条列出。"}], key=KEY)
    c2 = r["choices"][0]["message"]["content"]
    print(f"\n[Q2 细节] {dt:.1f}s finish={r['choices'][0]['finish_reason']}\n{c2}")

    # 第 3 问：更深处细节（仅新消息）
    r, dt = chat([{"role": "user", "content": "升级步骤一共几步？第二步具体做什么？另外 v1.4 相比 v1.3 的查询性能提升了多少？"}], key=KEY)
    c3 = r["choices"][0]["message"]["content"]
    print(f"\n[Q3 深挖] {dt:.1f}s finish={r['choices'][0]['finish_reason']}\n{c3}")

    # 第 4 问：文档尾部细节（数据字典 + 缺陷清单末尾 + 附录）
    r, dt = chat([{"role": "user", "content": "dwd_order 表里 order_amount_net 和 order_amount_gross 的区别是什么？DB-2210 修复的问题是什么？迁移看板的 dashboard id 是什么？"}], key=KEY)
    c4 = r["choices"][0]["message"]["content"]
    print(f"\n[Q4 尾部细节] {dt:.1f}s finish={r['choices'][0]['finish_reason']}\n{c4}")

    # 第 5 问：自动化生成区段的细节（重压长上下文召回）
    r, dt = chat([{"role": "user", "content": "ads_sales_daily 表的 aov 字段口径是什么？DB-2287 修复的是什么？ext_field_042 的类型和默认值是什么？"}], key=KEY)
    c5 = r["choices"][0]["message"]["content"]
    print(f"\n[Q5 尾部深挖] {dt:.1f}s finish={r['choices'][0]['finish_reason']}\n{c5}")

    # 校验关键事实（宽松匹配）
    checks = [
        ("Q2 版本号", c2, ["1.4"]),
        ("Q2 三个缺陷", c2, ["DB-2041", "DB-2107", "DB-2133"]),
        ("Q3 升级四步", c3, ["四", "4"]),
        ("Q3 性能提升", c3, ["31%"]),
        ("Q4 净额毛额", c4, ["净额", "毛额"]),
        ("Q4 DB-2210 分页跳行", c4, ["2210", "分页", "跳行"]),
        ("Q4 dashboard id", c4, ["migration-v14-progress"]),
        ("Q5 aov 口径", c5, ["客单价"]),
        ("Q5 DB-2287", c5, ["2287", "REPLACE TABLE"]),
        ("Q5 ext_field_042", c5, ["ext_field_042", "STRING"]),
    ]
    ok = True
    for name, text, keys in checks:
        hit = any(k in text for k in keys)
        print(f"[check] {name}: {'PASS' if hit else 'FAIL'}")
        ok = ok and hit
    print("\n长上下文验证:", "全部通过" if ok else "存在失败项")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
