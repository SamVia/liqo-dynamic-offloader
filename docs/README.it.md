# liqo-dynamic-offloader
*Leggi in altre lingue: [English](../README.md)*

Operatore Kubernetes event-driven per il recupero automatico dei workload che rimangono bloccati in `OffloadingBackOff` durante l'offloading Liqo verso un cluster remoto.

L'operatore osserva gli aggiornamenti dei pod e lo stato prodotto dal Virtual Kubelet di Liqo, abilita temporaneamente l'offloading del namespace coinvolto, forza il retry del workload e rimuove la configurazione quando non ci sono più pod remoti attivi.

## Panoramica

Liqo mantiene i namespace isolati finché non esiste una risorsa `NamespaceOffloading`. Se un pod viene indirizzato verso un virtual node remoto senza questa policy, il Virtual Kubelet rifiuta la reflection e il pod può manifestare il blocco `OffloadingBackOff`, nello stato del pod o nello stato waiting di un container.

`liqo-dynamic-offloader` risolve il problema senza richiedere configurazioni statiche per ogni namespace:

1. intercetta la creazione o l'aggiornamento di un pod in `OffloadingBackOff`;
2. crea la risorsa `NamespaceOffloading` con il cluster remoto configurato;
3. elimina il pod bloccato, lasciando al relativo controller Kubernetes il compito di ricrearlo;
4. osserva il ciclo di vita dei pod remoti;
5. revoca la policy quando non rimangono pod remoti attivi, ripristinando l'isolamento del namespace.

## Architettura

Il processo principale in `main.go` avvia un unico manager `controller-runtime` con due reconciler indipendenti.

### Trap Controller

Il **Trap Controller** osserva direttamente le risorse Kubernetes `Pod`. Il predicate lascia passare creazioni e aggiornamenti, mentre la riconciliazione applica gli early exit e verifica in modo autorevole lo stato corrente del pod. Un pod viene considerato intrappolato quando `OffloadingBackOff` compare in `status.reason`, nello stato waiting di un container o nello stato waiting di un init container.

Quando rileva il trap, ignora i namespace esclusi e i pod già in terminazione. Per gli altri namespace:

- crea `NamespaceOffloading/offloading` nel namespace del pod;
- configura `namespaceMappingStrategy: DefaultName`;
- usa `podOffloadingStrategy: LocalAndRemote`;
- vincola l'offloading tramite l'etichetta `liqo.io/remote-cluster-id` se in `TARGET_CLUSTER_ID` sono specificati dei cluster, altrimenti lascia la configurazione aperta per delegare il bilanciamento a Liqo;
- attende il periodo `TRAP_BACKOFF`, lasciando il tempo ai webhook Liqo di registrare la nuova policy;
- rilegge il pod dopo l'attesa e lo elimina con propagazione background solo se è ancora in `OffloadingBackOff`.

Se la policy esiste già, il controller rispetta il cooldown `TRAP_BACKOFF` dell'ultima remediation prima di procedere. Se Liqo ha già recuperato il pod durante il backoff, la cancellazione viene evitata. In caso contrario, il Deployment, ReplicaSet o altro workload controller può creare una nuova istanza dopo l'applicazione della policy.

### Cleanup Controller
Il **Cleanup Controller** osserva i pod e riconcilia solo i cambiamenti rilevanti per il conteggio dei workload remoti: creazione, eliminazione, cambio di fase, assegnazione del nodo o ingresso nel graceful shutdown.

Prima di contare i pod, il controller verifica l'età della `NamespaceOffloading`. Una policy appena creata non viene rimossa: il reconcile viene riprogrammato dopo il periodo `CLEANUP_GRACE_PERIOD`, così Liqo e il Trap Controller possono completare lo sblocco e la schedulazione.

Trascorso il periodo di grazia, per ogni namespace con una `NamespaceOffloading` considera attivi i pod che:

- sono assegnati al cluster remoto;
- richiedono il cluster remoto tramite `kubernetes.io/hostname`;
- sono ancora `Pending` ma destinati al cluster remoto;
- stanno terminando, anche se la cancellazione è già iniziata.

La policy viene eliminata soltanto quando il conteggio scende a zero. In questo modo il **Graceful Shutdown** viene rispettato: un pod in terminazione continua a mantenere l'offloading finché non è effettivamente uscito dal workload. Al completamento della pulizia il controller genera anche l'evento Kubernetes normal `Successful Cleanup`.

## Features

### Configurazione dinamica

L'operatore non hardcoda l'identificativo del cluster remoto né l'elenco dei namespace protetti. La configurazione viene letta all'avvio:

| Variabile | Descrizione | Default |
| --- | --- | --- |
| `TARGET_CLUSTER_ID` | Elenco separato da virgole degli ID Liqo dei cluster verso cui abilitare l'offloading. Se lasciato vuoto, la scelta del cluster viene delegata allo scheduler di Liqo. | *(vuoto)* |
| `EXCLUDED_NAMESPACES` | Elenco separato da virgole dei namespace da ignorare. Gli spazi vengono rimossi. | `kube-system,liqo-system,local-path-storage,crownlabs-system` |
| `TRAP_BACKOFF` | Durata dell'attesa dopo la creazione della policy e prima del ricontrollo del pod bloccato. | `2s` |
| `CLEANUP_GRACE_PERIOD` | Età minima della policy prima che il Cleanup Controller possa valutarne la revoca. | `10s` |

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

## Demo 2: Produzione In-Cluster (Docker)

Per testare l'operatore esattamente come girerebbe in un ambiente reale (es. CrownLabs), puoi buildare l'immagine e iniettarla nel cluster con un singolo script che si occupa anche di applicare i manifest RBAC e il Deployment:

```bash
make docker-build
./scripts/3-demo-complete.sh
```

Lo script configurerà i permessi (inclusa la Leader Election), avvierà il Pod dell'operatore, creerà la "trappola" per Liqo e mostrerà i log in streaming del recupero automatico.

Per ripristinare esplicitamente lo scenario iniziale:

```bash
./scripts/0-reset.sh
```

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

### Esempio di Deployment con Variabili d'Ambiente

Quando si distribuisce l'operatore in un cluster reale, è possibile iniettare la configurazione dinamica direttamente nelle specifiche del container utilizzando l'array `env`.

Ecco un esempio completo di un manifest Deployment Kubernetes che utilizza l'immagine GHCR e imposta le variabili di configurazione:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: liqo-dynamic-offloader
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: liqo-dynamic-offloader
  template:
    metadata:
      labels:
        app: liqo-dynamic-offloader
    spec:
      containers:
      - name: manager
        image: ghcr.io/samvia/liqo-dynamic-offloader:latest
        imagePullPolicy: Always
        env:
        # 1. Cluster di destinazione (separati da virgola). Lasciare vuoto per delegare il bilanciamento a Liqo.
        - name: TARGET_CLUSTER_ID
          value: "cluster-remote-1"
        # 2. Namespace protetti che l'operatore non toccherà mai
        - name: EXCLUDED_NAMESPACES
          value: "kube-system,liqo-system,local-path-storage,crownlabs-system,my-custom-ns"
        # 3. Timeout e periodi di grazia
        - name: TRAP_BACKOFF
          value: "2s"
        - name: CLEANUP_GRACE_PERIOD
          value: "10s"
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
    └── 3-demo-complete.sh
```

I documenti all'interno di `docs/` approfondiscono i dettagli tecnici architetturali e i log di collaudo avanzati raccolti durante lo sviluppo.

## Sicurezza e comportamento operativo

* I namespace di sistema esclusi non vengono modificati dall'operatore.
* Il Trap Controller non entra in hot-loop sui pod già in terminazione e non elimina un pod che Liqo ha già recuperato durante il backoff.
* Il Cleanup Controller non rimuove una policy prima del periodo di grazia né mentre esiste un pod remoto attivo o in terminazione.
* Gli errori di accesso al cluster o di cancellazione vengono restituiti dalla riconciliazione e ritentati secondo il comportamento standard di `controller-runtime`.
* Sono necessari i permessi Kubernetes per leggere e osservare i **Nodi** e i Pod, gestire risorse custom `NamespaceOffloading`, emettere eventi e gestire `Lease` per la Leader Election.
