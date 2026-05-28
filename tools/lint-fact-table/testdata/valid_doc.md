# Valid Doc

This document declares facts in a speech 表 and references them in body
without drift.

<!-- fact-table:start name="valid-doc" -->

| 维度 | 实测值 | 备注 |
|---|---|---|
| Hub struct 字段 | **47** | v0.6.1 |
| Server 行数 | **21313** | 不含测试 |

<!-- fact-table:end -->

## Body

Hub struct 共有 **47 字段**——这是 v0.6.1 实测值，Hub struct 字段对账一致。

Server 行数实测 **21313** 行（不含测试），按 §0 速查表对账。

## 边界情况

减幅 **-77%** <!-- lint:allow:derived-percentage --> 是从 21313→5000 推导，
不在 fact-table 里，用白名单豁免。
