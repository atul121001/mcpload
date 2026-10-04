### Compared with baseline

Baseline `4a1b2c3 (main)` · soak · 2025-06-18 · started 2026-09-29T13:00:00Z

**Performance regression detected:**

- search p95 372 ms → 500 ms (+34%, +128 ms)

| Tool | Calls | p50 | p95 | p99 | Error rate | req/s |
|---|--:|--:|--:|--:|--:|--:|
| `search` | 16,560 → 16,560 | 118 ms → 118 ms | 372 ms → 500 ms (+34%) ⚠ | 716 ms → 716 ms · | 0.02% → 0.02% · | 7.7 → 7.7 |
| `fast` | 9,936 → 9,936 | 11 ms → 11 ms | 29 ms → 29 ms · | 54 ms → 54 ms · | 0% → 0% · | 4.6 → 4.6 |
| `slow` | 3,312 → 3,312 | 303 ms → 303 ms | 331 ms → 331 ms · | 372 ms → 372 ms · | 0.06% → 0.06% · | 1.5 → 1.5 |
| `flaky` (too few calls) | 3,312 → 30 | 39 ms → 39 ms | 118 ms → 118 ms | 262 ms → 262 ms | 9.81% → 10% (+2%) | 1.5 → 0.0 |
| `new` (added) | – → 400 | – → 3.0 ms | – → 8.0 ms | – → 12 ms | – → 0% | – → 0.2 |
| `big` (removed) | 3,312 → – | 205 ms → – | 518 ms → – | 902 ms → – | 0% → – | 1.5 → – |

| Run | Baseline | Current | Δ | |
|---|--:|--:|--:|---|
| error rate | 0.75% | 0.75% | 0 pts | · |
| memory growth | 1.0 MiB | 1.0 MiB | 0 MiB | · |
| leak slope | 0.03 MiB/min | 0.03 MiB/min | 0 MiB/min | · |
| retained after cool-down | -5.2 MiB | -5.2 MiB | 0 MiB | · |

<sub>Rules: p95 +20% / p99 +30% and +25 ms; error rate +0.5 pts, +50% and p < 0.001; at least 50 calls per tool. ⚠ regression · ✓ improvement · · within noise</sub>
