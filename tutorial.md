1. Ho eseguito il file 1-setup
2. Eseguo manifest.yaml (pod forzato sul nodo liqo)
3. Analizzo gli errori
    - **Monitoraggio Avanzato dell'Errore:**
        Per analizzare come il sistema reagisce in fase di fallimento, possiamo monitorare tre componenti chiave:
        
        **A. Log del Virtual Kubelet di Liqo**
        Per vedere il momento esatto in cui il kubelet virtuale rifiuta il pod.
        - Trova il pod: `kubectl get pods -A | grep vk`
        - Leggi i log: `kubectl logs -n <NAMESPACE> <NOME_POD_VK> | grep -i -E "test-pod|offload"`

        **B. Eventi di Kubernetes**
        Per ispezionare la catena di eventi a livello di control-plane.
        - Dettagli pod: `kubectl describe pod test-pod -n test-liqo` (guarda la sezione Events in basso)
        - Storico eventi del namespace: `kubectl get events -n test-liqo --sort-by='.metadata.creationTimestamp'`

        **C. Metriche Esposte da Liqo (Prometheus)**
        Per dimostrare l'errore in modo empirico raccogliendo i counter degli errori.
        - Esponi le metriche (Port-forward): `kubectl port-forward -n liqo deploy/liqo-controller-manager 8082:8082`
        - Leggi i dati: In un altro terminale, lancia `curl -s http://localhost:8082/metrics | grep -i -E "liqo|offload|backoff"`

    - **Risultati dell'Analisi e Deduzioni Architetturali:**
        1. **La "Smoking Gun" negli Eventi:** L'evento `ReflectionDisabled` dimostra chiaramente che Liqo rifiuta il Pod *intenzionalmente* perché manca il `NamespaceOffloading`.
        2. **L'assenza di Metriche Custom:** Ispezionando Prometheus sulla porta 8082, vediamo solo metriche standard (`workqueue_work_duration_seconds`). Non esistono metriche custom per contare i pod bloccati. 
        3. **Scelta Architetturale (Event-Driven vs Polling):** Questo dimostra che l'unico modo nativo ed elegante per intercettare l'errore *senza introdurre latenza* (polling continuo) e *senza perdere contesto* (Prometheus non dice "quale" pod è bloccato) è sviluppare un Custom Controller che faccia il **Watch nativo sui Pod**. Questo approccio soddisfa i requisiti di isolamento ("No Malloppone") ed evita dipendenze da tool esterni come Prometheus.

4. **Sviluppo del Custom Controller (Fase di Scaffolding)**
    Abbiamo deciso di utilizzare **Kubebuilder** per generare l'infrastruttura del nostro operatore Kubernetes. Questa scelta ci garantisce un'architettura standard, nativa e robusta.
    
    - **Inizializzazione del progetto:**
      ```bash
      kubebuilder init --domain lico-helper.io --repo github.com/youngstef/liqo-stef-helper --skip-go-version-check
      ```
      *Cosa fa:* Inizializza il modulo Go e genera l'impalcatura base del progetto (es. `cmd/main.go`, `Makefile`, cartella `config/` con i manifesti). Il flag `--skip-go-version-check` è stato usato per la compatibilità con la versione locale di Go.

    - **Creazione dell'API (Il Controller):**
      ```bash
      kubebuilder create api --group core --version v1 --kind Pod --resource=false --controller=true
      ```
      *Cosa fa:* Genera lo scheletro del controller dentro `internal/controller/pod_controller.go` (con la funzione `Reconcile` vuota) e lo registra automaticamente nel Manager dentro `main.go`.
      *Dettaglio chiave:* Abbiamo usato `--resource=false` perché NON vogliamo creare una nuova Custom Resource Definition (CRD). Stiamo creando un controller in ascolto su una risorsa già esistente nativamente in Kubernetes (i `Pod` del gruppo `core`).

5. **Logica del Custom Controller (Implementazione)**
    Abbiamo inserito la logica operativa nel file `internal/controller/pod_controller.go` per identificare l'errore `OffloadingBackOff`. Ecco le scelte architetturali principali effettuate:

    - **Permessi RBAC (Role-Based Access Control):**
      Aggiunto il marker `// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch`. Di base un controller Pod non ha i privilegi per leggere i Namespace. Questo permesso permette al controller di leggere le etichette dei namespace per applicare il whitelisting in sicurezza.

    - **Whitelisting tramite "Early Exit" (Cache-based):**
      Piuttosto che implementare una funzione bloccante (Predicate) all'ingresso degli eventi, abbiamo implementato il pattern dell'Early Exit all'inizio della funzione `Reconcile`. Il controller recupera il Namespace del Pod e controlla la label `liqo-helper.io/monitor: "true"`. 
      *Motivazione architetturale:* L'API di Kubernetes non permette di filtrare i Pod in base alle label del loro Namespace genitore (limite cross-resource). Abbiamo evitato di fare il controllo della label in un Predicate custom perché le funzioni Predicate sono sincrone: fare una chiamata API lì bloccherebbe l'event-loop (l'Informer) rischiando colli di bottiglia e perdita di eventi (race conditions con la cache). Eseguendo il controllo all'interno della `Reconcile` in modo asincrono, il client sfrutta la Cache RAM locale di Kubebuilder a costo di rete zero e senza bloccare il flusso.

    - **L'Architettura del Rifiuto di Liqo (Global Pod Status vs Container Status):**
      Un pod va normalmente in errore a livello di Container (es. `CrashLoopBackOff` o `ImagePullBackOff`) solo *dopo* che lo Scheduler di Kubernetes gli ha assegnato fisicamente un nodo e il Kubelet prova ad avviarlo. Liqo, invece, agisce come un "Gatekeeper" avanzato (tramite webhook/virtual-kubelet): intercetta il pod *prima* dell'assegnazione al nodo remoto. Rifiutando la schedulazione alla radice per mancanza del permesso (`NamespaceOffloading`), i container non vengono mai creati e la lista `ContainerStatuses` resta vuota. L'errore `OffloadingBackOff` viene quindi "timbrato" da Liqo direttamente nello stato **Globale** del Pod (`pod.Status.Reason`). L'Operator è stato ottimizzato per interrogare questo campo globale, garantendo un'intercettazione fulminea.

    - **Ottimizzazione del Traffico (Predicates Base):**
      Nella funzione `SetupWithManager` abbiamo aggiunto `.WithEventFilter(predicate.ResourceVersionChangedPredicate{})`. Questo scudo di base scarta tutti gli eventi fittizi (es. i classici "resync" periodici dell'API server che inviano lo stato senza modifiche), svegliando la `Reconcile` solo a fronte di una reale mutazione del Pod.

    - **Validazione del Proof of Concept (Fail Fast):**
      Appena viene individuato il Pod bloccato in partenza dal Control Plane di Liqo, il controller stampa a terminale un log ad altissima precisione (`.000` millisecondi) contenente Nome Pod e Namespace. Questo dimostra empiricamente la latenza vicina allo zero dell'architettura Event-Driven prima di procedere con l'automazione.
