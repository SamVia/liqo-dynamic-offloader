<p align="center">
  <img src="../assets/logo.png" width="150" alt="Liqo Dynamic Offloader Logo">
</p>

# liqo-dynamic-offloader
*Leggi in altre lingue: [English](../README.md)*

Operatore Kubernetes event-driven per il recupero automatico dei workload che rimangono bloccati in `OffloadingBackOff` durante l'offloading Liqo verso un cluster remoto.

L'operatore osserva gli aggiornamenti dei pod e lo stato prodotto dal Virtual Kubelet di Liqo, abilita temporaneamente l'offloading del namespace coinvolto, forza il retry del workload e rimuove la configurazione quando non ci sono più pod remoti attivi.

## Panoramica

Liqo mantiene i namespace isolati finché non esiste una risorsa `NamespaceOffloading`. Se un pod viene indirizzato verso un virtual node remoto senza questa policy, il Virtual Kubelet rifiuta la reflection e il pod può manifestare il blocco `OffloadingBackOff`, nello stato del pod o nello stato waiting di un container.

`liqo-dynamic-offloader` risolve il problema senza richiedere configurazioni statiche per ogni namespace:

1. intercetta la creazione o l'aggiornamento di un pod in `OffloadingBackOff`;
2. crea la risorsa `NamespaceOffloading` con il cluster remoto configurato;
3. applica l'annotazione `dynamic-offloader.liqo.io/force-sync` al pod bloccato, richiedendo una nuova sincronizzazione senza eliminarlo;
4. osserva il ciclo di vita dei pod remoti;
5. avvia un conto alla rovescia persistente quando non rimangono pod remoti attivi e revoca la policy dopo il ritardo configurato.

## Architettura

Il processo principale in `main.go` avvia un unico manager `controller-runtime` con due reconciler indipendenti.

### Trap Controller

Il **Trap Controller** osserva direttamente le risorse Kubernetes `Pod`. Il predicate lascia passare creazioni e aggiornamenti, mentre la riconciliazione applica gli early exit e verifica in modo autorevole lo stato corrente del pod. Un pod viene considerato intrappolato quando `OffloadingBackOff` compare in `status.reason`, nello stato waiting di un container o nello stato waiting di un init container.

Quando rileva il trap, ignora i namespace esclusi e i pod già in terminazione. Per gli altri namespace:

- crea `NamespaceOffloading/offloading` nel namespace del pod;
- configura `namespaceMappingStrategy: DefaultName`;
- usa `podOffloadingStrategy: LocalAndRemote`;
- vincola l'offloading tramite l'etichetta `liqo.io/remote-cluster-id` se i cluster sono specificati con `--target-cluster-ids`, altrimenti delega la scelta a Liqo;
- attende il periodo `--trap-backoff`, lasciando il tempo alla logica asincrona di ammissione e schedulazione di Liqo di elaborare la nuova policy;
- rilegge il pod dopo l'attesa e applica l'annotazione `force-sync` solo se è ancora in `OffloadingBackOff`.

Se la policy esiste già, il controller rispetta il cooldown della remediation a livello di namespace prima di procedere. Se Liqo ha già recuperato il pod durante il backoff, non viene eseguita alcuna modifica. In modalità dry-run vengono registrate le azioni previste senza creare o aggiornare risorse.

### Cleanup Controller
Il **Cleanup Controller** osserva i pod e riconcilia solo i cambiamenti rilevanti per il conteggio dei workload remoti: creazione, eliminazione, cambio di fase, assegnazione del nodo o ingresso nel graceful shutdown.

Per ogni namespace con una `NamespaceOffloading` considera attivi i pod che:

- sono assegnati al cluster remoto;
- richiedono il cluster remoto tramite `kubernetes.io/hostname`;
- sono ancora `Pending` ma destinati al cluster remoto;
- stanno terminando, anche se la cancellazione è già iniziata.

Quando il conteggio scende a zero, il controller salva un timestamp `empty-since` e riprogramma la riconciliazione dopo `--cleanup-delay`. Se durante il conto alla rovescia compaiono nuovi workload remoti, il timestamp viene rimosso. Allo scadere del ritardo la policy viene eliminata. In questo modo il **Graceful Shutdown** viene rispettato: un pod in terminazione continua a mantenere l'offloading finché non è effettivamente uscito dal workload. Al completamento della pulizia il controller genera anche l'evento Kubernetes normal `Successful Cleanup`.

## Features

### Configurazione dinamica

L'operatore viene configurato all'avvio tramite flag della riga di comando. Il chart Helm mappa i valori sotto `config` su questi flag:

| Flag | Descrizione | Default |
| --- | --- | --- |
| `--target-cluster-ids` | Elenco separato da virgole degli ID Liqo. Se vuoto, la scelta viene delegata a Liqo. | *(vuoto)* |
| `--excluded-namespaces` | Namespace da ignorare, separati da virgole. Supporta pattern glob come `*-system`. | `kube-system,liqo-system` |
| `--trap-whitelist-labels` | Label `key=value` richieste sul namespace per abilitare il Trap Controller. | *(vuoto)* |
| `--trap-blacklist-labels` | Label `key=value` che disabilitano il Trap Controller sul namespace. | *(vuoto)* |
| `--cleanup-whitelist-labels` | Label `key=value` richieste sul namespace per abilitare il Cleanup Controller. | *(vuoto)* |
| `--cleanup-blacklist-labels` | Label `key=value` che disabilitano il Cleanup Controller sul namespace. | *(vuoto)* |
| `--trap-backoff` | Durata dell'attesa prima di ricontrollare un pod bloccato. | `2s` |
| `--cleanup-delay` | Conto alla rovescia prima di eliminare una policy vuota. `0` disabilita la pulizia automatica. | `10s` |
| `--dry-run` | Registra le azioni previste senza modificare lo stato del cluster. | `false` |

### High Availability

Il manager usa la **Leader Election** di `controller-runtime` con l'identificativo `liqo-auto-healing-lock` e salva il lock nel namespace `default` (tramite risorse `Lease`). È possibile eseguire più repliche dell'operatore: una sola replica diventa leader ed esegue la riconciliazione, mentre le altre restano pronte a subentrare in caso di failure.

### Osservabilita

Il Cleanup Controller pubblica metriche Prometheus nel registry di `controller-runtime`:

- `liqo_cleanup_namespaces_total`: contatore delle policy di offloading revocate con successo;
- `liqo_cleanup_active_remote_pods{namespace="..."}`: gauge del numero di pod remoti attivi o in graceful shutdown per namespace.

In aggiunta ai log strutturati, ogni cleanup completato produce un evento Kubernetes nativo con reason `Successful Cleanup`.

## Prerequisiti

- Docker
- [kind](https://kind.sigs.k8s.io/)
- `kubectl`
- `liqoctl` 1.0 o successivo
- Go 1.22 o successivo
- Bash, Git Bash o WSL per eseguire gli script `.sh` su Windows

Gli script creano due cluster kind denominati `cluster-local` e `cluster-remote`, installano Liqo e li collegano tramite un gateway NodePort.

## Setup dell'infrastruttura

Dalla root del repository, rendi eseguibili gli script e avvia il setup:

```bash
chmod +x scripts/*.sh
./scripts/1-setup.sh
```

Lo script attende la creazione del virtual node nel cluster locale e che diventi `Ready`. Al termine nessun namespace applicativo è ancora offloaded.

## Demo 1: Sviluppo Locale (`go run`)

Verificare che `kubectl` punti al cluster locale:

```bash
kubectl config use-context kind-cluster-local
```

In un terminale, avviare l'operatore dalla root del repository:

```bash
make run
```

In un secondo terminale, lanciare lo script di demo interattivo:

```bash
./scripts/2-demo.sh
```

## Demo 2: Produzione In-Cluster (Helm)

Per testare l'operatore in un ambiente reale, il chart Helm viene installato
utilizzando di default l'immagine remota GHCR:

```bash
./scripts/3-demo-complete.sh
```

Lo script continua a scaricare l'immagine
`ghcr.io/samvia/liqo-dynamic-offloader:latest` usando
`imagePullPolicy: Always`. È possibile sovrascrivere l'immagine senza
modificare lo script:

```bash
IMAGE_TAG=0.1.0 IMAGE_PULL_POLICY=IfNotPresent ./scripts/3-demo-complete.sh
```

Lo script installa o aggiorna la release Helm, configura i flag del controller,
crea namespace e workload di test e mostra i log dell'operatore.

Per ripristinare esplicitamente lo scenario iniziale:

```bash
./scripts/0-reset.sh
```

## Deployment Helm

Il metodo consigliato per gli ambienti di produzione è il chart Helm in
`charts/liqo-dynamic-offloader`. Il chart crea il Deployment dell'operatore,
il ServiceAccount, il ClusterRole e il ClusterRoleBinding.

Per validare e renderizzare il chart:

```bash
helm lint charts/liqo-dynamic-offloader
helm template liqo-dynamic-offloader charts/liqo-dynamic-offloader \
  --namespace liqo-system
```

Per installare o aggiornare l'operatore:

```bash
helm upgrade --install liqo-dynamic-offloader \
  charts/liqo-dynamic-offloader \
  --namespace liqo-system \
  --create-namespace \
  --set image.tag=0.1.0 \
  --set config.targetClusterIDs=cluster-remote
```

Per configurare i filtri basati sulle label, usare valori stringa:

```bash
helm upgrade --install liqo-dynamic-offloader \
  charts/liqo-dynamic-offloader \
  --namespace liqo-system \
  --create-namespace \
  --set-string config.trapBlacklistLabels=dynamic-offloader.liqo.io/ignore-trap=true \
  --set-string config.cleanupBlacklistLabels=dynamic-offloader.liqo.io/ignore-cleanup=true
```

Il supporto webhook è riservato a una versione futura ed è disabilitato.
Il controller attuale non espone endpoint, Service o integrazione con
certificati; mantenere `webhook.enabled` impostato su `false`.

## Test automatici

Il modulo Go contiene test unitari completi per reconciler e predicate event-driven:

```bash
go test -v ./...
```

I test coprono pod in `OffloadingBackOff`, policy già esistenti, namespace esclusi, pod in terminazione, pod remoti attivi, pod pending destinati al remoto, pod terminali e pod locali.

## Build e Containerizzazione

Per generare i manifest RBAC partendo dai marker Kubebuilder:

```bash
make manifests
```

Per compilare il binario locale in `bin/manager`:

```bash
make build
```

Per creare l'immagine Docker containerizzata:

```bash
make docker-build IMG=tuousername/liqo-dynamic-offloader:latest
```
### Immagini precompilate (Docker Hub & GHCR)

Se desideri utilizzare l'operatore senza compilarlo dal codice sorgente, puoi referenziare direttamente le immagini pubbliche ospitate su Docker Hub o GitHub Container Registry all'interno dei tuoi manifest o Deployment Kubernetes:

**Opzione 1: Docker Hub**

```yaml
    spec:
      containers:
      - name: manager
        image: samvia/liqo-dynamic-offloader:latest
```

**Opzione 2: GitHub Container Registry (GHCR)**

```yaml
    spec:
      containers:
      - name: manager
        image: ghcr.io/samvia/liqo-dynamic-offloader:latest
```

## Struttura del repository

```text
.
├── Dockerfile
├── Makefile
├── README.md
├── go.mod
├── go.sum
├── main.go
├── charts/
│   └── liqo-dynamic-offloader/
├── controllers/
│   ├── liqo_cleanup_controller.go
│   ├── liqo_cleanup_controller_test.go
│   ├── liqo_trap_controller.go
│   ├── liqo_trap_controller_test.go
│   └── predicates_test.go
├── config/
│   └── rbac/
│       └── role.yaml
├── docs/
│   ├── demo_advanced.md
│   ├── demo_base.md
│   ├── demo_complete.md
│   └── docs.md
└── scripts/
    ├── 0-reset.sh
    ├── 1-setup.sh
    ├── 2-demo.sh
    ├── 3-demo-complete.sh
    └── 4-locale.sh
```

I documenti all'interno di `docs/` approfondiscono i dettagli tecnici architetturali e i log di collaudo avanzati raccolti durante lo sviluppo.

## Sicurezza e comportamento operativo

* I namespace di sistema esclusi non vengono modificati dall'operatore.
* Il Trap Controller non entra in hot-loop sui pod già in terminazione e non modifica un pod che Liqo ha già recuperato durante il backoff.
* Il Cleanup Controller non rimuove una policy prima del periodo di grazia né mentre esiste un pod remoto attivo o in terminazione.
* Gli errori di accesso al cluster o di cancellazione vengono restituiti dalla riconciliazione e ritentati secondo il comportamento standard di `controller-runtime`.
* Sono necessari i permessi Kubernetes per leggere e osservare i **Nodi** e i Pod, gestire risorse custom `NamespaceOffloading`, emettere eventi e gestire `Lease` per la Leader Election.
