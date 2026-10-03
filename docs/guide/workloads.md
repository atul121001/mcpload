# Workload profiles

[README](../../README.md) · [All docs](../README.md)

Testing tools one by one tells you each tool is fast. A workload profile tells you whether the *jobs* your agents do stay fast: describe the flows, how often each one happens, the test data they use and the budget each must meet, in one YAML file.

```yaml
workload:
  name: customer-support
  agents: 20
  duration: 1m
  data:
    customers: { file: customers.csv }        # one row per agent session
  budgets: { p95: 2s, completion: 99% }       # every flow, end to end
  flows:
    - name: lookup-orders
      weight: 60
      steps:
        - { tool: search_customer, args: { email: "{{data.customers.email}}" }, as: customer }
        - { tool: get_orders, args: { customer_id: { $from: customer, path: id } } }
    - name: check-subscription
      weight: 30
      steps: [search_customer, get_subscription]
    - name: create-ticket
      weight: 10
      budgets: { p95: 3s, completion: 95% }
      steps: [search_customer, create_ticket]
```

```bash
mcpload run --url https://staging.example.com/mcp --workload customer-support.yaml --html report.html
```

Each agent session picks a flow by weight, runs its steps (calls can run in parallel and use earlier results, as in `agent-workflow`), pauses between steps like an agent deciding what to do, and closes. A flow counts as completed only if every call in it succeeded. mcpload checks the whole file before it starts and points at the exact flow, step and field of any mistake.

[examples/workloads/customer-support.yaml](../../examples/workloads/customer-support.yaml) maps these business flows onto the demo servers' tools. Here it is against the demo servers for 30 seconds with 20 agents. The healthy server:

```text
mcpload workload customer-support (3 flows, 1029 runs; end-to-end times of completed flows):
  flow                weight    runs  completed       p50       p95       p99  slowest step (p95)
  lookup-orders          60%     601     100.0%    543 ms    1.46 s    1.81 s  get_order_details 13 ms
  check-subscription     30%     323     100.0%    196 ms    865 ms    1.41 s  search_customer 4.0 ms
  create-ticket          10%     105      99.0%    1.23 s    2.03 s    2.31 s  create_ticket 705 ms
  PASS     workload           All 3 flows of `customer-support` held their budgets (closest: `lookup-orders` p95 1.46 s of 2 s).
mcpload result: PASS
```

The same profile against the server whose tool calls share a pool of 2 slots, so lookups wait behind ticket writes:

```text
  lookup-orders          60%     407     100.0%    891 ms    2.11 s    2.74 s  get_orders 620 ms
  check-subscription     30%     213     100.0%    439 ms    1.45 s    1.83 s  get_subscription 591 ms
  create-ticket          10%      66     100.0%    1.56 s    2.56 s    3.02 s  create_ticket 1.29 s
  FAIL     workload           flow `lookup-orders` p95 2.11 s > 2 s budget. flow `create-ticket` step `create_ticket` p95 1.29 s > 1 s budget.
mcpload result: FAIL
```

Start from [examples/workloads/template.yaml](../../examples/workloads/template.yaml), which explains every option. The full format is in [scenarios/README.md](../../scenarios/README.md#workload-profiles).
