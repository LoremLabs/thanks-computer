# Quickstart

Build your first intelligence matrix with [Thanks, Computer](https://www.thanks.computer) in minutes.

An intelligence matrix is a system that remembers what it is trying to accomplish. As opportunities, requests, approvals, and conversations move through the system, context stays attached and goals remain visible.

Let's see one running.


## 1. Install

```sh
brew tap loremlabs/txco && brew install txco
```

or

```sh
curl -fsSL https://get.thanks.computer/install.sh | bash
```

## 2. See it run

```sh
txco demo
```

This boots a throwaway local **chassis** — the TxCo runtime,
one binary — and opens a demo in your browser. 

![The txco demo playground in the browser](https://github.com/user-attachments/assets/696fce36-17ae-4609-807d-723450a3c6bc)


## 3. How stacks work

Every intelligence matrix is composed of stacks. Each stack contributes a small piece of behavior, context, or decision-making.

As your work moves through the matrix, context travels with it. Most operations ignore most events. Only the operations whose conditions match will react, contribute information, and influence what happens next.

```stack
opportunity

50 mission okrs history
200 score review
250 assign
300 followup
```

When an opportunity enters the matrix, mission, OKRs, and history contribute context. Other operations score, review, assign, and follow up. Together they help the system make decisions that remain aligned with its goals.

Three ideas make this possible:

- **Read JSON. Write JSON.** An operation is anything that accepts JSON and returns JSON. Any HTTP service in any language can participate.
- **Shared context, not shared state.** Operations coordinate by contributing to the same event document rather than calling each other directly.
- **Only relevant operations react.** Each operation is gated by a condition. Most operations ignore most events.
  
A complete operation is a few lines:

```txcl
WHEN @web.req.url.path == "/opportunity"
EXEC "https://api.example.com/enrich-opportunity"
```

