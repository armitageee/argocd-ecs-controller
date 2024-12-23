# argocd-ecs-controller
ArgoCD external cluster secret controller

***Контроллер для добавления downstream кластеров argocd***

Для локального запуска сбилдить контейнер и запустить со следующими параметрами:
```shell
docker run -d \                                                         
  -e NAMESPACE=argocd \
  -v ${HOME}/.kube/config:/root/.kube/config \
  {{ image-name }}
```
В волюме указать путь до кубконфига кластера в котором планируется запускать контроллер

Для работы внутри кластера необходимо создать role и role binding с соответствующими правами и прокинуть поду переменную `$NAMESPACE` с указанным нсом внутри которого расположена argocd
## Метрики
Prometheus метрики доступны по /metrics на порту 2112
Из кастомных метрик есть:
- secrets_added_total
- secrets_updated_total
- secrets_deleted_total
## External-secrets-operator
Корректнее всего использовать в связке с ESO, через external-secrets ссылаетесь на секрет с нужным кубконфигом и вешаете `label: cluster`, далее создается обычный секрет с этим лейблом и уже дальше контроллер создает кластерный секрет, при обновлении секрета в vault или удалении, секрет будет автоматически удален.
## Принцип работы
Контроллер следит за секретами внутри NS, где расположена argocd, при первом запуске он считывает все секреты внутри неймспейса, если среди них был секрет с `label: cluster`, то забирает из него кубконфиг и создает новый секрет для argocd внутрь которого вставляет токен и url кластера, если создается новый секрет, то контроллер его считывает и делает все то же самое, если удаляем секрет с кубконфигом, то целевой секрет удаляется вместе с ним
### TODO:
Дописать хелм чарт и раскатку в целевой кластер и сборку бинарника с пушем в наш реджистри(в целом для тестов все темплейты уже валидны)
### Пример работы

![update_secret.png](docs%2Fupdate_secret.png)
