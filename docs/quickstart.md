# Quickstart

Build your first workflow with [Thanks, Computer](https://www.thanks.computer) in minutes.

Thanks, Computer is an event-driven runtime for durable, human-in-the-loop workflows. As requests, emails, approvals, and conversations move through it, context stays attached — and an operation can pause for a human (or a slow AI), then resume exactly once.

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

This boots a self-contained local **chassis** — the TxCo runtime,
one binary — and opens a guided demo in your browser, with short tracks for web, mail, async (human-in-the-loop), and MCP. 

![The txco demo playground in the browser](https://github.com/user-attachments/assets/696fce36-17ae-4609-807d-723450a3c6bc)


## 3. How stacks work

A workflow is composed of **stacks**. Each stack contributes a small piece of behavior, context, or decision-making.

As your work moves through, context travels with it. Most operations ignore most events. Only the operations whose conditions match will react, contribute information, and influence what happens next.

```stack
opportunity

50 mission okrs history
200 score review
250 assign
300 followup
```

When an opportunity enters, mission, OKRs, and history contribute context. Other operations score, review, assign, and follow up. Together they help the system make decisions that remain aligned with its goals.

Three ideas make this possible:

- **Read JSON. Write JSON.** An operation is anything that accepts JSON and returns JSON. Any HTTP service in any language can participate.
- **Shared context, not shared state.** Operations coordinate by contributing to the same event document rather than calling each other directly.
- **Only relevant operations react.** Each operation is gated by a condition. Most operations ignore most events.
  
A complete operation is a few lines:

```txcl
WHEN @web.req.url.path == "/opportunity"
EXEC "https://api.example.com/enrich-opportunity"
```

