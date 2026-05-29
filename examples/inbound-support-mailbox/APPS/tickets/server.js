// Toy ticketing service — for the inbound-support-mailbox example.
//
// Receives the full chassis envelope on POST /tickets and prints
// the fields that matter for a triage row. Replace with whatever
// real ticket store you run (Linear, Zendesk, JIRA, a Postgres
// table, etc.) when you wire this for production.
const http = require('node:http')

const PORT = 4200

const server = http.createServer((req, res) => {
    if (req.url === '/health') {
        res.writeHead(200, { 'Content-Type': 'text/plain' })
        res.end('ok\n')
        return
    }
    if (req.method !== 'POST' || req.url !== '/tickets') {
        res.writeHead(404, { 'Content-Type': 'text/plain' })
        res.end('not found\n')
        return
    }

    let body = ''
    req.on('data', (c) => (body += c))
    req.on('end', () => {
        let env = {}
        try {
            env = JSON.parse(body || '{}')
        } catch (e) {
            res.writeHead(400, { 'Content-Type': 'text/plain' })
            res.end(`bad json: ${e.message}\n`)
            return
        }

        const lmtp = env?._txc?.lmtp ?? {}
        const ticket = env?._txc?.ticket ?? {}

        // What a real implementation would persist.
        const row = {
            from: lmtp.mail?.from,
            to: lmtp.rcpt?.[0],
            subject: lmtp.msg?.subject,
            category: ticket.category,
            priority: ticket.priority,
            rid: env?._txc?.rid,
            received_at: env?._ts,
        }

        // Log loudly so the example is satisfying when you tail it.
        console.log('TICKET', JSON.stringify(row))

        res.writeHead(200, { 'Content-Type': 'application/json' })
        res.end(JSON.stringify({ ok: true, ticket_id: env?._txc?.rid }))
    })
})

server.listen(PORT, () => {
    console.log(`tickets listening on :${PORT}`)
})
