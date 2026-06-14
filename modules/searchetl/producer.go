package searchetl

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/contract/searchmsg"
	"github.com/segmentio/kafka-go"
)

// producer 把抽取出的正文契约投递到 Kafka topic octo.message.v1（正文流）/ DLQ topic
// octo.message.v1.dlq（真异常）。
//
// 设计纪律：
//   - Kafka key = message_id：保证同一消息恒进同一分区，配合 ES _id=message_id upsert，
//     得到 sink 处的 effectively-once（**注意：这是 at-least-once 上线 + ES 幂等 sink，
//     不是 opanalytics 的同事务累加器机制，二者不等价**，C2 语义口径硬要求）。
//   - 事务边界由调用方（ETL.RunIncremental）保证：producer 只在读事务**之外**被调用，
//     绝不持 DB 锁做 Kafka 网络 IO。
//   - RequireAll（acks=-1）：等所有 ISR 确认才算投递成功，避免 leader 切换丢消息。
type producer struct {
	writer    *kafka.Writer
	dlqWriter *kafka.Writer
	topic     string
	dlqTopic  string
}

// newProducer 按配置建 Kafka writer。仅在 Kafka.On 时调用——off 时 ETL 不构造 producer、
// 不连 Kafka（保持阶段 1 惰性）。
func newProducer(cfg *config.Config) *producer {
	common := func(topic string) *kafka.Writer {
		return &kafka.Writer{
			Addr:     kafka.TCP(cfg.Kafka.Brokers...),
			Topic:    topic,
			Balancer: &kafka.Hash{}, // 按 key(message_id) 哈希分区，保证同消息同分区
			// RequireAll：等所有 ISR ack，最强持久化保证（at-least-once 的前提）。
			RequiredAcks: kafka.RequireAll,
			// 同步投递：Write 返回即代表 broker 已确认（或失败）；ETL 据此决定是否推进游标。
			Async:        false,
			BatchTimeout: 50 * time.Millisecond,
			WriteTimeout: 10 * time.Second,
			// 允许首投时自动创建 topic（生产由部署侧预建并配 retention，这里兜底本地/预发）。
			AllowAutoTopicCreation: true,
		}
	}
	return &producer{
		writer:    common(cfg.Kafka.Topic),
		dlqWriter: common(cfg.Kafka.DLQTopic),
		topic:     cfg.Kafka.Topic,
		dlqTopic:  cfg.Kafka.DLQTopic,
	}
}

// produceBatch 把一批正文契约整批投递到正文 topic。**整 chunk 原子语义**：任一条投递失败
// 即返回 error，调用方据此**不推进游标**、下轮整批重投（靠 message_id 幂等去重，C2）。
//
// 入参 msgs 已是稳定前缀内、outcome=outcomeOK/outcomeRawExcluded 的正文消息（raw_excluded
// 也走正文流，content=null）。DLQ 消息由 produceDLQ 单独处理。
func (p *producer) produceBatch(ctx context.Context, msgs []searchmsg.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	kmsgs := make([]kafka.Message, 0, len(msgs))
	for i := range msgs {
		b, err := json.Marshal(msgs[i])
		if err != nil {
			// 契约序列化失败属编码 bug（非数据问题），整批失败不推进，避免漏投。
			return fmt.Errorf("searchetl: marshal message %s: %w", msgs[i].MessageID, err)
		}
		kmsgs = append(kmsgs, kafka.Message{
			Key:   []byte(msgs[i].MessageID),
			Value: b,
		})
	}
	if err := p.writer.WriteMessages(ctx, kmsgs...); err != nil {
		return fmt.Errorf("searchetl: produce batch to %s: %w", p.topic, err)
	}
	return nil
}

// produceDLQ 把一条真异常消息投到 DLQ topic（保留可见性字段 + message_id 供排查）。
// DLQ 投递失败同样返回 error 让整 chunk 不推进——绝不允许「DLQ 写失败→静默丢」。
func (p *producer) produceDLQ(ctx context.Context, msgs []searchmsg.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	kmsgs := make([]kafka.Message, 0, len(msgs))
	for i := range msgs {
		b, err := json.Marshal(msgs[i])
		if err != nil {
			return fmt.Errorf("searchetl: marshal dlq message %s: %w", msgs[i].MessageID, err)
		}
		kmsgs = append(kmsgs, kafka.Message{
			Key:   []byte(msgs[i].MessageID),
			Value: b,
		})
	}
	if err := p.dlqWriter.WriteMessages(ctx, kmsgs...); err != nil {
		return fmt.Errorf("searchetl: produce batch to dlq %s: %w", p.dlqTopic, err)
	}
	return nil
}

// Close 关闭底层 writer 连接。
func (p *producer) Close() error {
	var firstErr error
	if p.writer != nil {
		if err := p.writer.Close(); err != nil {
			firstErr = err
		}
	}
	if p.dlqWriter != nil {
		if err := p.dlqWriter.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
