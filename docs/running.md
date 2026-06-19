# Production Deploys

You can self-host a [Thanks, Computer](https://www.thanks.computer) runtime ("the chassis"), or
use the hosted cloud service.

## Self Hosting

You can start a TxCo chassis with:

```sh
txco serve
```

When it prints `-ready-`, it's listening: `:8080` for web events,
`:5050` for TCP, `:8081` for the admin API, and a cron tick every 60s.
No database to provision, no containers — state lives in local files
next to the binary.


## Enroll your keys

Production chassis auth uses signed requests. Every admin call carries an ed25519 signature, with replay protection built in.

Enrolling is one command on a fresh chassis:

:::note
On first boot you validate yourself as an admin by entering a code from the logs. 

Subsequent auth uses public key signatures. 

:::


```sh
txco auth bootstrap-local    # one-time: enroll your signing key
txco auth login              # opens the admin UI, authenticated
```

