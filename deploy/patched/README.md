# 自建修补镜像（GitHub Actions → GHCR）

上游 `engine/llm` 的流式请求只发送 `Accept: text/event-stream`，部分 OpenAI 兼容网关
（如 vLLM 系，报 `406 {"detail":"Could not satisfy the request Accept header."}`）
要求 Accept 中必须包含 `application/json`。本目录用 fork 分支 `fix/openai-stream-accept`
的源码构建自用镜像，生产部署不再依赖官方镜像。

## 方式 A（推荐）：GitHub Actions 自动构建

fork 里的 `selfhost` 分支（已合并两个修复）带了 `.github/workflows/patched-image.yml`，
推送该分支即触发构建并推送到 GHCR：

- `ghcr.io/gtqbhksl/antifox:latest`
- `ghcr.io/gtqbhksl/antifox:patched`
- `ghcr.io/gtqbhksl/antifox:patched-<sha>`

查看进度：仓库 **Actions → selfhost-image**。首次跑完后去
**个人主页 → Packages → antifox → Package settings → Change visibility → Public**，
服务器拉镜像就不需要登录；不想公开则在服务器上
`docker login ghcr.io -u gtqbhksl`（PAT 勾 `read:packages`）。

重新构建：向 `selfhost` 分支推一个提交，或在 Actions 里对该工作流
「Re-run all jobs」。

## 部署

把现有 `docker-compose.yml` 的镜像行改成你的仓库：

```yaml
image: ghcr.io/gtqbhksl/antifox:${ANTIFOX_TAG:-latest}
```

或叠加覆盖文件（项目目录仍取第一个 `-f` 文件所在目录，`./data`、`./skills` 不受影响）：

```bash
docker compose -f docker-compose.yml -f deploy/patched/docker-compose.override.yml pull antifox
docker compose -f docker-compose.yml -f deploy/patched/docker-compose.override.yml up -d
```

`.env` 里保持 `ANTIFOX_TAG=latest` 即可（本仓库的 latest 指向最近一次 Actions 构建）；
需要复现某个版本时固定成 `patched-<sha>`。注意 `latest` 跟着本仓库的构建走，
**不会**因为上游发版自动更新——要跟上游同步需先合并上游代码再触发一次构建。
不要用上游的版本号 tag，本仓库未发布。

## 方式 B（备用）：本地构建

需要 Docker；磁盘预留约 5 GB；构建时需能访问 npm / Go module / apt / Google Fonts。

```bash
docker build -f deploy/patched/Dockerfile.local -t antifox:patched .
# 服务器上没有构建条件时：
docker save antifox:patched | gzip > antifox-patched.tar.gz
gunzip -c antifox-patched.tar.gz | docker load
```

ARM 服务器用 buildx：

```bash
docker buildx build --platform linux/arm64 \
  -f deploy/patched/Dockerfile.local -t antifox:patched --load .
```

## 验证

1. 打开 `http://<host>:8787`，进入「系统配置 → LLM 配置」；
2. Base URL 填 `https://api.ai.50x.energy/v1`，填 API Key 和模型 ID，点「测试连接」；
3. 预期返回成功和延迟。

## 已知边界

- 该网关高峰期会返回 `503 gateway_overloaded`（"the API is at capacity right now"），
  与 Accept 头无关，项目自带 3 次重试，仍失败只能错峰重试。
- 官方镜像的「一键更新」在容器内本就禁用（提示拉新镜像）；换成自制镜像后，
  更新方式改为重跑 Actions 并 `docker compose pull`。
