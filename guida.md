# Guida Rapida alla Verifica dell'Operatore Liqo-Helper

Questa guida ti permetterà di testare in prima persona l'efficacia del nostro Custom Controller. L'obiettivo è dimostrare come il controller, basato su architettura Event-Driven, intercetti **all'istante** un Pod che viene bloccato da Liqo.

Per goderti al meglio l'effetto, ti consigliamo di avere due terminali affiancati.

### Passo 1: Preparazione dell'Ambiente
Per prima cosa, dobbiamo tirare su l'infrastruttura di base (due cluster Kind e il peering tramite Liqo).

Esegui questo comando nel **Terminale 1**:
```bash
./1-setup.sh
```
*(Attendi qualche minuto affinché i cluster siano pronti e il peering di Liqo sia attivo).*

---

### Passo 2: Accensione del Controller (Il Vigile Urbano)
Ora accendiamo il nostro operatore. Lui si connetterà al cluster e si metterà in ascolto, ignorando tutto il traffico irrilevante.

Sempre nel **Terminale 1**, esegui:
```bash
make run
```
Vedrai dei log di avvio e poi si fermerà in attesa. Lascia questo terminale aperto e visibile.

---

### Passo 3: Il Trigger in Diretta!
Ora andiamo a creare il "problema" nel cluster. Applicheremo un Pod configurato per essere schedulato sul nodo remoto di Liqo, ma all'interno di un namespace (`test-liqo`) che **non** è stato esportato. Liqo rifiuterà il Pod mettendolo in stato di `OffloadingBackOff`.

Apri il **Terminale 2** ed esegui:
```bash
kubectl apply -f manifest.yaml
```

**Guarda subito il Terminale 1!**
In tempo quasi zero (nell'ordine dei millisecondi), vedrai il nostro controller accorgersi dell'errore e stampare questo messaggio:
```
INFO	TRIGGER FAIL-FAST: Pod in OffloadingBackOff intercettato!	{"pod": "test-pod", "namespace": "test-liqo"}
```

Questo dimostra la reattività assoluta del paradigma "A Posteriori" (Event-Driven) scelto per questo progetto.
