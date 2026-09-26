<p align="center"><img src="docs/site/src/assets/logo.svg" alt="Das Ptah-Logo: ein bernsteinfarbener Deckstein über zwei hellblauen Steinlagen auf einem dunklen Quadrat mit abgerundeten Ecken" width="72" height="72"></p>

<h1 align="center">Ptah Operator</h1>

<p align="center">Eine Kubernetes-Steuerungsebene, die PostgreSQL- und MySQL-Schemas anhand unveränderlicher OCI-Artefakte abgleicht.</p>

<p align="center"><a href="README.md">English</a> · <a href="README.ja.md">日本語</a> · <strong>Deutsch</strong> · <a href="README.fr.md">Français</a></p>

<p align="center">
  <a href="https://github.com/stokaro/ptah-operator/actions/workflows/ci.yml?query=branch%3Amaster"><img src="https://img.shields.io/github/actions/workflow/status/stokaro/ptah-operator/ci.yml?branch=master&label=ci&logo=github" alt="Status des CI-Workflows auf master: Quellcodeprüfung, Race Detector und vollständiger Kubernetes-Lebenszyklus"></a>
  <a href="https://github.com/stokaro/ptah-operator/actions/workflows/demo.yml?query=branch%3Amaster"><img src="https://img.shields.io/github/actions/workflow/status/stokaro/ptah-operator/demo.yml?branch=master&label=demonstration&logo=github" alt="Status des wöchentlichen Demo-Workflows auf master, der jedes Szenario in einem echten Cluster neu aufzeichnet"></a>
  <a href="https://github.com/stokaro/ptah-operator/actions/workflows/docs.yml?query=branch%3Amaster"><img src="https://img.shields.io/github/actions/workflow/status/stokaro/ptah-operator/docs.yml?branch=master&label=docs&logo=github" alt="Status des Dokumentations-Workflows auf master"></a>
  <a href="https://github.com/stokaro/ptah-operator/blob/master/LICENSE"><img src="https://img.shields.io/github/license/stokaro/ptah-operator?label=license&color=blue" alt="Lizenz: MIT"></a>
  <a href="https://github.com/stokaro/ptah-operator/blob/master/go.mod"><img src="https://img.shields.io/github/go-mod/go-version/stokaro/ptah-operator?label=go%20%E2%89%A5&logo=go&logoColor=white" alt="Die in go.mod angegebene niedrigste Go-Version, mit der sich dieses Modul kompilieren lässt"></a>
</p>

<p align="center"><a href="https://operator.ptah.run/edge/start/install/">Installation</a> · <a href="https://operator.ptah.run/edge/start/first-schema/">Erstes Schema</a> · <a href="https://operator.ptah.run/demo/">Aufgezeichnete Abläufe</a> · <a href="https://operator.ptah.run/">Dokumentation</a> · <a href="https://operator.ptah.run/support/ptah/">Ptah-Kompatibilität</a></p>

Ptah Operator ist eine Kubernetes-native Steuerungsebene, die PostgreSQL- und
MySQL-Schemas fortlaufend mit unveränderlichen OCI-Artefakten abgleicht.
Datenbankoperationen laufen in kurzlebigen, gehärteten Jobs. Der Controller
selbst darf keine Datenbank-Secrets lesen.

`PtahSchema` gleicht die Datenbankstruktur und die in einer Deklaration benannten
Referenzdaten ab. `PtahMigration` führt eine vorbereitete Migrationsfolge aus
und prüft den aufgezeichneten Verlauf gegen das ausgewählte Artefakt, auch beim
Start von einem Checkpoint. Beide unterstützen PostgreSQL und MySQL.

Die API ist derzeit `v1alpha1`. Die End-to-End-Matrix ist grün: alle unterstützten
Kubernetes-Minorversionen, beide Datenbanken und beide Artefaktformate. Bis aus
dieser Implementierungsvorschau eine veröffentlichte Version wird, fehlt noch
ein Release. Was ein Lauf geprüft hat und mit welchem Ptah-Build, steht in
[`support/ptah.json`](support/ptah.json).

## Abgleichmodell

```text
resolve tag to digest -> verify artifact -> observe database -> publish plan
          ^                                                          |
          |                 approval (when required) <---------------+
          |                                                          |
          +--- verify convergence <- apply exact approved plan <-----+
```

Vor jedem Job reserviert der Controller eine Ausführung. Pro `PtahSchema`
erlaubt er höchstens einen aktiven Job und nimmt die Reservierung nach einem
Neustart wieder auf. Ein beendeter Prozess belegt keine Konvergenz: Auf jede Anwendung
folgt eine neue, ausschließlich lesende Beobachtung.

Sicherheitseigenschaften:

- OCI-Tags werden einmal aufgelöst. Alle weiteren Artefaktzugriffe verwenden den Digest.
- Pläne binden die exakten Bytes an Artefakt, Ziel, beobachteten Zustand, Richtlinie,
  per Digest festgelegtes Manager-Image, Manager-Revision und Zustandssemantik,
  Ptah-Version, Executor-Image, Runner-Image und Runner-Protokoll.
- Freigaben sind eigene unveränderliche Ressourcen. Die Admission-Prüfung
  versieht sie mit der authentifizierten Identität.
- Destruktive Pläne sind standardmäßig deaktiviert. Auch nach der Aktivierung
  benötigen sie eine Freigabe für genau diesen Plan.
- Zugangsdaten für Datenbank und Registry bleiben voneinander getrennt. Sie
  erscheinen nie in Status, Events, Planressourcen oder Befehlsargumenten.
- Löschen und Pausieren führen kein Bereinigungs-SQL aus.
- Bei ungewissem Anwendungsergebnis folgt eine Beobachtung statt einer Wiederholung.

## Das angewendete SQL lesen

Das SQL liegt in unveränderlichen ConfigMaps, die ein Plan über Index, Größe
und Digest bindet. Es steht weder im Status noch im Log. `kubectl ptah` liest es
auf dieselbe Weise wie der Operator:

```sh
kubectl ptah plan storefront --applied -n application -o sql
```

Dieser ausschließlich lesende Client wird mit jedem Release als `kubectl`-Plugin
für die unterstützten Clientplattformen veröffentlicht.
[Einen Plan lesen](https://operator.ptah.run/edge/use/read-a-plan/#install)
beschreibt die Installation und die benötigten RBAC-Berechtigungen
im jeweiligen Namespace.

## Installation und erstes Schema

Die Anleitung steht auf [operator.ptah.run](https://operator.ptah.run/).
Sie behandelt die Installation, die drei ausdrücklich anzugebenden Chart-Werte, ein vollständiges Schemabeispiel, Konfiguration, Betrieb,
Sicherheitsmodell, Condition-Gründe und Supportzeiträume.

```sh
cd docs/site && npm ci && npm run build
```

Dieser Befehl baut die Dokumentation aus diesem Checkout.

## Abläufe ansehen

Die [aufgezeichneten Abläufe](https://operator.ptah.run/demo/) zeigen
Terminalsitzungen mit einem echten Cluster: ein Schema anwenden und ändern,
einen Plan freigeben, eine destruktive Änderung ablehnen, Drift beheben sowie
Fehler und Wiederherstellung. Bei der Aufzeichnung werden die beschriebenen
Conditions an den tatsächlichen Objekten geprüft. Nur wenn alle Prüfungen
bestehen, wird die Aufzeichnung veröffentlicht.

Das Badge `demonstration` zeigt die wöchentliche Neuaufzeichnung auf `master`.
Es bezieht sich nicht auf die im Browser abgespielten Sitzungen. Grün bedeutet,
dass alle Szenarien in einem aus `master` gebauten Cluster weiterhin bestehen.
Die sichtbare Aufzeichnung ist in `demo/recordings/runs.json` hinterlegt und
ändert sich erst bei einer neuen Aufnahme. Ein rotes Badge zeigt daher, dass
eine Demonstration nicht mehr reproduzierbar ist.

[`demo/`](demo/README.md) erklärt auf Englisch den Aufbau und die eigene Ausführung.

## Dokumentation

Die englische Anleitung wird auf
[operator.ptah.run](https://operator.ptah.run/) veröffentlicht. Ihre Quellen
liegen in `docs/site/src/content/docs`. Eine Seite richtet sich an Personen,
die den Code ändern:

- [Architektur](https://operator.ptah.run/edge/reference/architecture/):
  Komponenten, ihre Speicherorte und ihre jeweiligen Invarianten.

Ptah selbst, seine Schemaformate, das OCI-Artefaktlayout und die CLI sind auf
[docs.ptah.run](https://docs.ptah.run/edge/) dokumentiert. Die
[Kompatibilitätsmatrix](https://operator.ptah.run/support/ptah/) der
Anleitung nennt die mit diesem Operator geprüften Ptah-Builds. Beide Ressourcen sind auf Englisch.

`PtahMigration` und `PtahSchema` bleiben getrennt. Eine deklarierte Zeilenmenge
beschreibt die gewünschten Daten, eine Migrationsfolge einen Zustandsübergang.
Deshalb haben beide eigene APIs und Zustandsautomaten. Sie teilen sich den
OCI-Transport, die Trennung der Zugangsdaten, das Ausführungsprotokoll und die
Koordination der Datenbankziele.

## Lizenz und Hilfe

Ptah Operator wird unter der [MIT-Lizenz](LICENSE) veröffentlicht.

Fragen und Fehlerberichte gehören in ein Issue bei
[stokaro/ptah-operator](https://github.com/stokaro/ptah-operator/issues).
Verhalten der eigenständigen Ptah-CLI gehört dagegen zu
[stokaro/ptah](https://github.com/stokaro/ptah/issues).
[CONTRIBUTING.md](CONTRIBUTING.md) beschreibt, was ein verwertbarer Bericht
enthalten muss und welche Prüfungen eine Änderung bestehen muss. Für die
Teilnahme gilt der [Verhaltenskodex](CODE_OF_CONDUCT.md). Diese Ressourcen sind
auf Englisch. Geschäftliche Anfragen gehen an `ask@stokaro.com`.
