# Kubergo

Учебный проект: Go-сервис с детекцией аномалий, развернутый в Kubernetes с мониторингом, алертами и HPA.

---

## Описание

`kubergo` — HTTP-сервис на Go, принимающий метрики нагрузки (RPS, CPU), вычисляющий скользящее среднее и определяющий аномалии на основе z-score.
Сервис развернут в Kubernetes, использует Redis для хранения данных, экспортирует метрики в Prometheus и масштабируется с помощью HPA.

---

## Архитектура

- Go HTTP API (`/api/v1/metrics`, `/api/v1/analyze`)
- Redis — хранение метрик и результатов анализа
- Kubernetes:
  - Deployment + Service для kubergo
  - Deployment + Service для Redis
  - HPA по CPU
- Monitoring:
  - Prometheus
  - Grafana
  - Alertmanager

---

## API

### Healthcheck
```http
GET /healthz
```

### Приём метрик
```http
POST /api/v1/metrics
Content-Type: application/json

{
  "timestamp": 0,
  "cpu": 10,
  "rps": 100
}
```

Ответ:
```json
{ "status": "accepted" }
```

### Анализ (детекция аномалий)
```http
GET /api/v1/analyze?debug=true
```

---

## Детекция аномалий

- Окно: 50 значений
- Метод: z-score
- Аномалия: `z-score > ANOMALY_THRESHOLD`
- Порог задаётся через переменную окружения:
```bash
ANOMALY_THRESHOLD=0.5
```

---

## Метрики Prometheus

Экспортируются следующие метрики:

- `kubergo_http_requests_total`
- `kubergo_http_request_duration_seconds`
- `kubergo_metrics_accepted_total`
- `kubergo_metrics_dropped_total`
- `kubergo_anomalies_total`

---

## Мониторинг и алерты

- Prometheus установлен через Helm
- Grafana с 3 дашбордами:
  1. Traffic (RPS)
  2. Latency
  3. Anomalies
- Alertmanager:
  - Алерт `KubergoHighAnomalyRate`
  - Срабатывает при >5 аномалий в минуту

---

## HPA

Horizontal Pod Autoscaler настроен на:

- minReplicas: 2
- maxReplicas: 5
- target CPU utilization: 70%

Масштабирование подтверждено под нагрузкой.

---

## Структура проекта

```
kubergo/
├── cmd/kubergo/main.go
├── deploy/
│   ├── redis/
│   ├── hpa/
│   ├── monitoring/
│   └── load/
├── Dockerfile
├── go.mod
├── go.sum
└── README.md
```

---

## Сборка образа

```bash
docker build -t kubergo:1.0 .
```

(Для локального Kubernetes образ импортируется в containerd.)

---

## Запуск в Kubernetes

Все манифесты находятся в каталоге `deploy/`.

Пример:
```bash
kubectl apply -f deploy/redis/
kubectl apply -f deploy/
kubectl apply -f deploy/hpa/
kubectl apply -f deploy/monitoring/
```

---

## Назначение проекта

Учебный проект для демонстрации:

- Go backend
- асинхронной обработки данных
- Kubernetes deployment
- мониторинга и алертинга
- автоскейлинга


# Команды для запуска в Kubernetes
```bash
kubectl -n app port-forward svc/redis 6379:6379
kubectl -n monitoring get pods -o wide
```

## Запуск Grafana & Prometheus & AlertManager с доступом по публичному IP
```bash
kubectl -n monitoring port-forward --address 0.0.0.0 svc/monitoring-grafana 3000:80
kubectl -n monitoring port-forward --address 0.0.0.0 svc/monitoring-kube-prometheus-prometheus 9090:9090
kubectl -n monitoring port-forward --address 0.0.0.0 svc/monitoring-kube-prometheus-alertmanager 9093:9093
kubectl -n app port-forward svc/kubergo 18080:80
```

## Перезапуск пода для применения изменений в main.go
### Пересобрать Docker-образ
```bash
cd ~/kubergo
sudo docker build -t kubergo:1.0 .
```
### Импортировать образ в containerd (k8s)
```bash
sudo docker save kubergo:1.0 | sudo ctr -n k8s.io images import -
```
### Перезапустить Deployment kubergo
```bash
kubectl -n app rollout restart deploy/kubergo
```
### Проверить статус роллаута
```bash
kubectl -n app rollout status deploy/kubergo
```
### Проверить, что pod обновился
```bash
kubectl -n app get pods -l app=kubergo -o wide
```

## Создание нагрузки через yaml
```bash
kubectl apply -f ~/kubergo/deploy/load/loadgen.yaml
kubectl -n app get pods -l app=kubergo-loadgen -o wide
```
## Выключение нагрузки
```bash
kubectl -n app delete deploy/kubergo-loadgen
```