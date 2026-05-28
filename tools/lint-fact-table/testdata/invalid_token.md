# Invalid Token Doc

This document drifts in body — fact-table says Hub 47 but body says 43.

<!-- fact-table:start name="invalid-token" -->

| 维度 | 实测值 |
|---|---|
| Hub struct 字段 | **47** |

<!-- fact-table:end -->

## Body

Hub struct **43 字段**——这是 v0.5 旧值，应该 fail lint：与 fact-table 的 47 不一致。
