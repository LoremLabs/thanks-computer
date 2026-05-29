// stripe-mock — a stand-in for api.stripe.com for the
// stripe-customer-enrich example. No dependencies (node:http built-in).
//
// It does two things:
//   1. Proves the secret round-tripped: it 401s unless the request
//      carries `Authorization: Bearer sk_…` — which the chassis only
//      produces by materializing STRIPE_API_KEY from the secret store
//      and substituting it via the rule's WITH clause.
//   2. Returns a canned customer for the id the chassis forwards
//      (read from `customer_id` in the posted envelope), mimicking
//      GET https://api.stripe.com/v1/customers/{id}.

const http = require('node:http')

const PORT = 4242

// Canned "database" — keyed by the customer id the sample webhook carries.
const CUSTOMERS = {
  cus_demo123: { id: 'cus_demo123', email: 'ada@example.com', name: 'Ada Lovelace' },
}

const server = http.createServer((req, res) => {
  if (req.url === '/health') {
    res.writeHead(200, { 'content-type': 'text/plain' })
    return res.end('ok')
  }

  let body = ''
  req.on('data', (chunk) => (body += chunk))
  req.on('end', () => {
    // The Authorization header is the proof the secret was materialized
    // and substituted by the chassis. We only check its shape here —
    // a real API would validate the key.
    const auth = req.headers['authorization'] || ''
    if (!auth.startsWith('Bearer sk_')) {
      res.writeHead(401, { 'content-type': 'application/json' })
      return res.end(JSON.stringify({ error: 'missing or malformed Authorization (expected Bearer sk_…)' }))
    }

    let id = ''
    try {
      id = JSON.parse(body || '{}').customer_id || ''
    } catch {
      // fall through to the not-found path below
    }

    const customer = CUSTOMERS[id]
    if (!customer) {
      res.writeHead(404, { 'content-type': 'application/json' })
      return res.end(JSON.stringify({ error: 'no such customer', customer_id: id }))
    }

    res.writeHead(200, { 'content-type': 'application/json' })
    res.end(JSON.stringify({ customer }))
  })
})

server.listen(PORT, () => {
  console.log(`stripe-mock listening on http://localhost:${PORT}`)
})
