# Cadence Forge · 音视频成片服务

Go HTTP 后端，以真实音视频字节为输入，提供版本化剪辑、字幕时间映射、MP4/WAV加工和可核验交付。没有Web前端。

## 运行

需要 Go 1.24+、Python 3（仅运行有界工作流检查）及带 libx264、AAC、loudnorm、tile、showwavespic 的 FFmpeg/ffprobe。当前本机实际验证 Go 1.27、FFmpeg/ffprobe 9.0.2；Docker构建定义采用Go 1.24和Debian媒体包，镜像运行尚需目标环境独立验证。

```sh
go build -trimpath -o bin/forged ./cmd/forged
./bin/forged
```

默认监听 `127.0.0.1:8734`，数据根 `var/cadence-forge`。可以设置 `FORGE_ADDR`、`FORGE_ROOT`、`FORGE_WORKERS`（1..8，默认2）、`FFMPEG_BIN`、`FFPROBE_BIN`。同一数据根仅允许一个服务进程。服务作为受信任环境中的媒体后端运行，面向公网接入前应部署认证和网络边界。

使用现有媒体工具运行所有真实工作流：

```sh
python3 scripts/workflow_check.py --workflow all
```

检查在临时数据根内生成4秒校准音画，不依赖外部素材或在线服务，完成22条工作流，包括真实强制终止及恢复、分片续编与交付包核验。`--workflow`可选择运行清单中的某条工作流；所需的前置接收、版本编辑和渲染也会执行。初始化基线不含自动测试套件，testing为deferred；这里提供有界生产工作流核验，不声明测试命令。

## 调用顺序

1. `POST /forge/v1/sources?name=校准片`：请求体为媒体原始字节，返回素材ID、SHA-256与流事实。
2. `POST /forge/v1/recipes`：例如 `{"name":"审片","format":"mp4","width":640,"height":360,"fps":24,"sample_rate":48000,"normalize":false,"target_lufs":-16}`。
3. `POST /forge/v1/sheets`：`{"name":"剪辑","spans":[{"source_id":"素材ID","in_ms":0,"out_ms":1500,"gain_db":0}],"cues":[{"source_id":"素材ID","start_ms":500,"end_ms":1000,"text":"校准"}]}`。
4. `GET /forge/v1/sheets/{id}/preview?recipe_id={id}`：预览当前最新修订的累计区间及WebVTT。片段按半开区间求交；同源重复出现分别映射。
5. `POST /forge/v1/renders`：提供 `Idempotency-Key`，正文 `{"sheet_id":"剪辑ID","sheet_revision":1,"recipe_id":"配方ID","recipe_revision":1}`。必须显式选择修订，提交冻结引用与时间计划；预览最新版本不替代提交选择。
6. `GET /forge/v1/renders/{id}`：查看排队、运行、成功、失败或取消及尝试历史。
7. `GET /forge/v1/renders/{id}/artifact`：下载检验通过的媒体；`?captions=1`下载字幕。`POST .../verify`重新核验；`GET .../bundle`返回媒体、字幕及冻结说明组成的ZIP。

## 多清晰度自适应分片交付

配方可在MP4上声明清晰度梯子，从而选择分片交付：

```json
{"name":"审片梯子","format":"mp4","width":640,"height":360,"fps":24,"sample_rate":48000,
 "normalize":false,"target_lufs":-16,"segment_ms":2000,
 "renditions":[{"name":"proxy","width":320,"height":180,"video_bitrate_kbps":400,"audio_bitrate_kbps":64},
               {"name":"final","width":640,"height":360,"video_bitrate_kbps":1200,"audio_bitrate_kbps":128}]}
```

`segment_ms`为1000..6000、步进100ms且必须有梯子；最多4个清晰度，名称唯一，尺寸不超出配方画幅。未声明`renditions`的配方保持单文件`artifact`行为。

提交时一次确定所有清晰度共享的规范时间边界（`boundaries`），任何一方都不得从磁盘文件自行推断完成度。清单是唯一事实：

- `GET /forge/v1/renders/{id}/delivery`返回已确认分片清单、每个清晰度可播放的**连续前缀**及`complete`。没有已确认首片的清晰度标记为`pending`，不会显示为可切换。
- `GET /forge/v1/renders/{id}/segments/{rendition}/{index}`只提供已登记分片。非清单内、未确认或已取消的分片一律409，即使磁盘上存在同名文件。
- `GET /forge/v1/renders/{id}/package`仅在清单完整且逐片摘要复核通过后返回ZIP：各清晰度全部按索引连续、时间边界一致，加字幕与`delivery.manifest.json`。不完整时409。

每个已确认分片记录其`digest`、`bytes`、规范起止毫秒和`submission`（冻结提交摘要）。审片者拿到前部即可开始检查，不需要等待较慢支路；完整包接收者必然得到边界一致、无缺片的全套清晰度。

编码策略：先为每个清晰度产出首片，使各档尽早可切换；随后按由低到高的清晰度顺序补齐。每个分片关闭后立即检验、fsync并原子发布，再登记清单，因此临时空间只需容纳单支路当前分段。

失败、取消与重试都核对同一份清单：进程中断后已确认前缀保留，显式重试从第一个缺失边界续编，不重复登记；取消确认后分片、包与核验均拒绝。之后的剪辑或配方修订不改变已冻结提交，`submission`仍是原值。

## 编辑与恢复

`PUT /sheets/{id}`及`PUT /recipes/{id}`使用十进制修订号 `If-Match: 1`。本服务的该头为应用级修订约定，并非通用HTTP条件请求实现。保存只追加历史，过期写入409。输入中的服务管理ID与修订由服务生成。

`POST /renders/{id}/cancel`撤回排队或运行中的交付；成功确认后该任务不能新提交成片。进程停止是后续动作，并不承诺确认瞬间资源已全部释放。成功成片不可再取消。`POST /renders/{id}/retry`仅接受failed，最多三次尝试，沿用原冻结输入。重启时running变为failed/interrupted，等待显式重试，queued继续排队。

`GET /events?after=0`返回至多200条严格后继事件。序号是持久顺序，时间戳仅用于解释。`GET /state`返回当前一致快照，适合受信任客户端。

## 分析入口

`POST /sources/{id}/loudness`测量源LUFS、真峰值和动态范围，静音或不可测结果使用null并指示silent。`GET /sources/{id}/visual?mode=storyboard`生成四格图；`mode=waveform`生成640×120单声道概览。它们是有成本的同步分析，请求受独立超时约束；波形合并声道可能隐藏反相内容，故事板不等于逐帧审片。

## 支持范围与限制

单素材最多64 MiB、约120秒、3840×2160和8声道。剪辑最多32段、120秒、500条字幕，段长至少100ms；输出MP4/H.264/AAC或WAV/16位PCM，采样率44.1或48kHz。MP4要求每段有视频；无声视频生成对应静音。WAV要求每段有声音。视频保持完整画面并留边，统一像素比例与帧率，声音统一立体声。归一化为单遍响度滤镜，不保证任何短片都精确达到目标值。分片交付仅限MP4，边界由成片时长与`segment_ms`决定，末尾过短片段并入前一片。

时长基准为毫秒，编码后的帧与采样可能有容差。检验检查流、尺寸、采样率、时长与摘要，不代表内容正确、逐帧同步或主观质量已通过人工审片。分片检验按规范边界逐片核对时长、画幅、采样率与摘要。禁止容器中的远程协议与非白名单封装，但媒体进程仍需要部署侧隔离。

接收先耐久保留字节再探测登记，失败可能留下未引用对象。成功产物先检验、耐久提升，再提交可见事实；崩溃可能留下未登记产物或已发布但未登记的分片。清单之外的文件永远不被提供，但当前不自动回收这些内容。`DELETE /sources/{id}`只退休无历史引用的素材记录，保守保留对象；历史剪辑、字幕和渲染血缘均阻止退休。

状态写入使用同文件系统临时文件、fsync、替换与父目录同步；目录同步失败时服务拒绝后续写入并要求恢复核对。对象及产物与状态分阶段提交，已生成目录不能自行视为交付成功。启动时校验清单与冻结提交、规范边界和梯子一致，损坏或被改写引用其他提交的清单会被拒绝。请备份整个数据根，并在停止唯一服务实例后进行一致复制。

## 容器

```sh
docker build -t cadence-forge .
docker run --rm -p 8734:8734 -v cadence-forge-data:/data cadence-forge
```

运行身份为10001。这里的容器定义不是跨平台运行通过声明；实际验证范围与本机工作流结果见交付记录。
