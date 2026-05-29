import { describe, it, expect, vi, afterEach } from 'vitest'
import {
    listSecrets,
    createSecret,
    rotateSecret,
    updateSecretDescription,
    revokeSecret,
    SecretStoreUnavailableError,
    SecretExistsError,
    SecretNotFoundError,
    SecretNameImmutableError,
} from './api'

function jsonResponse(status: number, body: unknown): Response {
    return {
        ok: status >= 200 && status < 300,
        status,
        statusText: '',
        json: async () => body,
    } as Response
}

afterEach(() => {
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
})

describe('listSecrets', () => {
    it('returns the secrets array on 200', async () => {
        const f = vi.fn().mockResolvedValue(jsonResponse(200, { secrets: [{ name: 'A' }] }))
        vi.stubGlobal('fetch', f)
        expect(await listSecrets('acme')).toEqual([{ name: 'A' }])
        expect(f.mock.calls[0][0]).toBe('/v1/tenants/acme/secrets')
    })

    it('maps 503 secret_store_unavailable to the typed error', async () => {
        vi.stubGlobal(
            'fetch',
            vi.fn().mockResolvedValue(jsonResponse(503, { error: 'secret_store_unavailable' }))
        )
        await expect(listSecrets('acme')).rejects.toBeInstanceOf(SecretStoreUnavailableError)
    })

    it('returns [] for an empty tenant without fetching', async () => {
        const f = vi.fn()
        vi.stubGlobal('fetch', f)
        expect(await listSecrets('')).toEqual([])
        expect(f).not.toHaveBeenCalled()
    })
})

describe('createSecret', () => {
    it('POSTs name/value/description and returns metadata', async () => {
        const f = vi
            .fn()
            .mockResolvedValue(jsonResponse(201, { secret: { name: 'STRIPE_KEY', key_version: 1 } }))
        vi.stubGlobal('fetch', f)

        const got = await createSecret('acme', 'STRIPE_KEY', 'sk_live', 'desc')
        expect(got).toMatchObject({ name: 'STRIPE_KEY' })

        const [url, init] = f.mock.calls[0]
        expect(url).toBe('/v1/tenants/acme/secrets')
        expect(init.method).toBe('POST')
        expect(JSON.parse(init.body as string)).toEqual({
            name: 'STRIPE_KEY',
            value: 'sk_live',
            description: 'desc',
        })
    })

    it('maps 409 to SecretExistsError', async () => {
        vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(409, { error: 'secret_exists' })))
        await expect(createSecret('acme', 'DUP', 'v', '')).rejects.toBeInstanceOf(SecretExistsError)
    })
})

describe('rotateSecret', () => {
    it('POSTs to /{name}/rotate with the value', async () => {
        const f = vi
            .fn()
            .mockResolvedValue(jsonResponse(200, { secret: { name: 'A', key_version: 2 } }))
        vi.stubGlobal('fetch', f)

        await rotateSecret('acme', 'A', 'newval')
        const [url, init] = f.mock.calls[0]
        expect(url).toBe('/v1/tenants/acme/secrets/A/rotate')
        expect(JSON.parse(init.body as string)).toEqual({ value: 'newval' })
    })

    it('maps 404 to SecretNotFoundError', async () => {
        vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(404, { error: 'secret_not_found' })))
        await expect(rotateSecret('acme', 'GONE', 'v')).rejects.toBeInstanceOf(SecretNotFoundError)
    })
})

describe('updateSecretDescription', () => {
    it('PATCHes only the description — never name', async () => {
        const f = vi.fn().mockResolvedValue(jsonResponse(200, { secret: { name: 'A' } }))
        vi.stubGlobal('fetch', f)

        await updateSecretDescription('acme', 'A', 'new desc')
        const [url, init] = f.mock.calls[0]
        expect(url).toBe('/v1/tenants/acme/secrets/A')
        expect(init.method).toBe('PATCH')
        const body = JSON.parse(init.body as string)
        expect(body).toEqual({ description: 'new desc' })
        expect(body).not.toHaveProperty('name')
    })

    it('maps 400 name_immutable to SecretNameImmutableError', async () => {
        vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(400, { error: 'name_immutable' })))
        await expect(updateSecretDescription('acme', 'A', 'x')).rejects.toBeInstanceOf(
            SecretNameImmutableError
        )
    })
})

describe('revokeSecret', () => {
    it('DELETEs and resolves on 204', async () => {
        const f = vi.fn().mockResolvedValue(jsonResponse(204, null))
        vi.stubGlobal('fetch', f)
        await expect(revokeSecret('acme', 'A')).resolves.toBeUndefined()
        expect(f.mock.calls[0][1].method).toBe('DELETE')
    })

    it('maps 404 to SecretNotFoundError', async () => {
        vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(404, { error: 'secret_not_found' })))
        await expect(revokeSecret('acme', 'GONE')).rejects.toBeInstanceOf(SecretNotFoundError)
    })
})
