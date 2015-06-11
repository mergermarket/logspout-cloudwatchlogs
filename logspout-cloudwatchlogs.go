package logspout

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/fsouza/go-dockerclient"
	"github.com/gliderlabs/logspout/router"
	"log"
	"strings"
	"time"
)

const envPrefix = "LOGSPOUT_CLOUDWATCHLOGS_LOG_GROUP="
const stdoutEnvPrefix = "LOGSPOUT_CLOUDWATCHLOGS_LOG_GROUP_STDOUT="
const stderrEnvPrefix = "LOGSPOUT_CLOUDWATCHLOGS_LOG_GROUP_STDERR="
const bufferMessagesPerContainerStream = 1024
const maxBatchWaitMilliseconds = 3 * 1000
const maxBackoffIntervalMilliseconds = 256 * 1000

func init() {
	router.AdapterFactories.Register(NewCloudWatchLogsAdapter, "cloudwatchlogs")
}

type CloudWatchLogsAdapter struct {
	cwl                  *cloudwatchlogs.CloudWatchLogs
	cw                   *cloudwatch.CloudWatch
	metricNamespace      string
	metricDimensions     *[]*cloudwatch.Dimension
	route                *router.Route
	streamPrefix         string
	defaultLogGroup      string
	logGroupFromEnv      bool
	containerLogGroups   map[string]LogGroups
	containerStreams     map[string]*containerStream
	eventsDropped        float64
	throttlingExceptions float64
	otherErrors          float64
}

type LogGroups struct {
	Both   string
	Stderr string
	Stdout string
}

func NewCloudWatchLogsAdapter(route *router.Route) (router.LogAdapter, error) {
	cwl := cloudwatchlogs.New(&aws.Config{
		Region: route.Address,
	})
	var cw *cloudwatch.CloudWatch
	if route.Options["metric-namespace"] != "" {
		cw = cloudwatch.New(&aws.Config{
			Region: route.Address,
		})
	}
	for k := range route.Options {
		if k != "stream-prefix" && k != "default-log-group" && k != "log-group-from-env" && k != "container-log-groups" && k != "metric-namespace" && k != "metric-dimensions" {
			return nil, errors.New(fmt.Sprintf("unknown option %s", k))
		}
	}
	if route.Options["stream-prefix"] == "" {
		return nil, errors.New("stream-prefix is a required option")
	}
	var logGroupFromEnv bool
	logGroupFromEnvParameter, logGroupFromEnvSet := route.Options["log-group-from-env"]
	if !logGroupFromEnvSet || logGroupFromEnvParameter == "off" {
		logGroupFromEnv = false
	} else if logGroupFromEnvParameter == "on" {
		logGroupFromEnv = true
	} else {
		return nil, errors.New("invalid value for log-group-from-env - must be \"on\" or \"off\" if present")
	}
	var containerLogGroups map[string]LogGroups
	if val, ok := route.Options["container-log-groups"]; ok {
		if err := json.Unmarshal([]byte(val), &containerLogGroups); err != nil {
			return nil, errors.New(fmt.Sprintf("error parsing container-log-groups %s", err))
		}
		for name := range containerLogGroups {
			if containerLogGroups[name].Both != "" && (containerLogGroups[name].Stdout != "" || containerLogGroups[name].Stderr != "") {
				return nil, errors.New(fmt.Sprintf("container-log-groups.%s must container either \"both\" _or_ \"stdout\" and/or \"stderr\""))
			}
		}
	}
	var dimensions map[string]string
	var metricDimensions []*cloudwatch.Dimension
	if val, ok := route.Options["metric-dimensions"]; ok {
		if err := json.Unmarshal([]byte(val), &dimensions); err != nil {
			return nil, errors.New(fmt.Sprintf("error parsing metric-dimensions %s", err))
		}
		metricDimensions = make([]*cloudwatch.Dimension, len(dimensions))
		i := 0
		for name, value := range dimensions {
			metricDimensions[i] = &cloudwatch.Dimension{
				Name:  aws.String(name),
				Value: aws.String(value),
			}
			i++
		}
	} else {
		metricDimensions = []*cloudwatch.Dimension{}
	}
	return &CloudWatchLogsAdapter{
		cwl:                  cwl,
		cw:                   cw,
		route:                route,
		streamPrefix:         route.Options["stream-prefix"],
		defaultLogGroup:      route.Options["default-log-group"],
		metricNamespace:      route.Options["metric-namespace"],
		metricDimensions:     &metricDimensions,
		logGroupFromEnv:      logGroupFromEnv,
		containerLogGroups:   containerLogGroups,
		eventsDropped:        0,
		throttlingExceptions: 0,
		otherErrors:          0,
		containerStreams:     make(map[string]*containerStream),
	}, nil
}

func (self *CloudWatchLogsAdapter) putMetric(name string, value float64) {
	_, err := self.cw.PutMetricData(
		&cloudwatch.PutMetricDataInput{
			MetricData: []*cloudwatch.MetricDatum{
				{
					MetricName: aws.String(name),
					Dimensions: *self.metricDimensions,
					Value:      &value,
				},
			},
			Namespace: aws.String(self.metricNamespace),
		},
	)
	if err != nil {
		log.Printf("error putting %s metric to cloudwatch: %s", name, err)
	}
}

func (self *CloudWatchLogsAdapter) sendMetrics() {
	ticker := time.NewTicker(time.Minute)
	for {
		select {
		case <-ticker.C:
			if self.metricNamespace != "" {
				self.putMetric("EventsDropped", self.eventsDropped)
				self.putMetric("ThrottlingExceptions", self.throttlingExceptions)
				self.putMetric("OtherErrors", self.otherErrors)
			}
			log.Printf("dropped events: %.0f, throttlingExceptions: %.0f, otherErrors: %.0f",
				self.eventsDropped,
				self.throttlingExceptions,
				self.otherErrors,
			)
			self.eventsDropped = 0
			self.throttlingExceptions = 0
			self.otherErrors = 0
		}
	}
}

func (self *CloudWatchLogsAdapter) Stream(logstream chan *router.Message) {
	go self.sendMetrics()
	for m := range logstream {
		name := strings.Join([]string{self.streamPrefix, m.Container.Name[1:], m.Source}, "-")
		id := strings.Join([]string{m.Container.ID, m.Source}, "-")
		var stream = self.containerStreams[id]
		if stream == nil {
			stream = self.createContainerStream(name, m)
			self.containerStreams[id] = stream
		}
		if stream.channel != nil {
			event := &cloudwatchlogs.InputLogEvent{
				Message:   aws.String(fmt.Sprintf("%s\n", m.Data)),
				Timestamp: aws.Long(m.Time.UnixNano() / (1000 * 1000)),
			}
			select {
			case stream.channel <- event:
			default:
				self.eventsDropped++
			}
		}
	}
}

func (self *CloudWatchLogsAdapter) createContainerStream(name string, firstMessage *router.Message) *containerStream {
	logGroupName, logGroupNameSource := self.getLogGroupName(firstMessage.Container, firstMessage.Source)
	var channel chan *cloudwatchlogs.InputLogEvent
	var streamInitChannel chan *string
	if logGroupName != "" {
		channel = make(chan *cloudwatchlogs.InputLogEvent, bufferMessagesPerContainerStream)
		streamInitChannel = make(chan *string, 1)
	}
	stream := containerStream{
		logGroupName:       logGroupName,
		logGroupNameSource: logGroupNameSource,
		channel:            channel,
		streamInitChannel:  streamInitChannel,
		adapter:            self,
		name:               name,
	}
	if stream.channel != nil {
		go stream.initLogStream(streamInitChannel)
		go stream.handleMessages(firstMessage)
	}
	return &stream
}

func (self *CloudWatchLogsAdapter) getLogGroupName(container *docker.Container, source string) (string, string) {
	if logGroups, explicitLogGroups := self.containerLogGroups[container.Name[1:]]; explicitLogGroups {
		if logGroups.Both != "" {
			return logGroups.Both, fmt.Sprintf("container-log-groups.%s.both parameter", container.Name[1:])
		} else if source == "stdout" {
			return logGroups.Stdout, fmt.Sprintf("container-log-groups.%s.stdout parameter", container.Name[1:])
		} else if source == "stderr" {
			return logGroups.Stderr, fmt.Sprintf("container-log-groups.%s.stderr parameter", container.Name[1:])
		} else {
			return "", ""
		}
	} else if self.logGroupFromEnv {
		for i := 0; i < len(container.Config.Env); i++ {
			if source == "stdout" && strings.HasPrefix(container.Config.Env[i], stdoutEnvPrefix) {
				return container.Config.Env[i][len(stdoutEnvPrefix):], fmt.Sprintf("%s.%s environment variable", container.Name[1:], stdoutEnvPrefix[:len(stdoutEnvPrefix)-1])
			} else if source == "stderr" && strings.HasPrefix(container.Config.Env[i], stderrEnvPrefix) {
				return container.Config.Env[i][len(stderrEnvPrefix):], fmt.Sprintf("%s.%s environment variable", container.Name[1:], stderrEnvPrefix[:len(stderrEnvPrefix)-1])
			} else if strings.HasPrefix(container.Config.Env[i], envPrefix) {
				return container.Config.Env[i][len(envPrefix):], fmt.Sprintf("%s.%s environment variable", container.Name[1:], envPrefix[:len(envPrefix)-1])
			}
		}
	}
	return self.defaultLogGroup, "default-log-group parameter"
}

type containerStream struct {
	logGroupName       string
	logGroupNameSource string
	name               string
	channel            chan *cloudwatchlogs.InputLogEvent
	streamInitChannel  chan *string
	adapter            *CloudWatchLogsAdapter
}

func (self *containerStream) getLogStream() (*string, error) {
	logStreamsDescription, descErr := self.adapter.cwl.DescribeLogStreams(&cloudwatchlogs.DescribeLogStreamsInput{
		LogGroupName:        &self.logGroupName,
		LogStreamNamePrefix: &self.name,
	})
	if descErr != nil {
		return nil, errors.New(fmt.Sprintf(
			"could not check for (describe) log stream \"%s\" for log group \"%s\" (from %s): %s",
			self.name, self.logGroupName, self.logGroupNameSource, descErr,
		))
	}
	if len(logStreamsDescription.LogStreams) == 0 {
		_, createErr := self.adapter.cwl.CreateLogStream(&cloudwatchlogs.CreateLogStreamInput{
			LogGroupName:  &self.logGroupName,
			LogStreamName: &self.name,
		})
		if createErr != nil {
			return nil, errors.New(fmt.Sprintf(
				"could not create log stream \"%s\" for log group \"%s\" (from %s): %s",
				self.name, self.logGroupName, self.logGroupNameSource, createErr,
			))
		}
		return nil, nil
	} else {
		if *logStreamsDescription.LogStreams[0].LogStreamName != self.name {
			return nil, errors.New(fmt.Sprintf("unexpected log stream \"%s\" matching \"%s\" (%d streams returned) in log group \"%s\" (from %s)",
				*logStreamsDescription.LogStreams[0].LogStreamName, self.name,
				len(logStreamsDescription.LogStreams), self.logGroupName, self.logGroupNameSource,
			))
		}
		return logStreamsDescription.LogStreams[0].UploadSequenceToken, nil
	}
}

func (self *containerStream) initLogStream(done chan *string) {
	var interval time.Duration = 1000 / 8
	for {
		sequenceToken, err := self.getLogStream()
		if err != nil {
			log.Print(fmt.Sprintf("error getting log stream (backing off for %f second(s)): %s", interval/1000, err))
			time.Sleep(interval * time.Millisecond)
			interval *= 2
			if interval > maxBackoffIntervalMilliseconds {
				interval = maxBackoffIntervalMilliseconds
			}
		} else {
			done <- sequenceToken
			break
		}
	}
}

// currently conservative to avoid hitting these limits:
// http://docs.aws.amazon.com/AmazonCloudWatchLogs/latest/APIReference/API_PutLogEvents.html
// TODO: track batch size in bytes and use maximum possible limits (batches will fail if messages are on average greater than 1,048,576 / 512 - 26 = 2,022)
const maxBatchSize = 512

func (self *containerStream) handleMessages(firstMessage *router.Message) {
	var batch [maxBatchSize]*cloudwatchlogs.InputLogEvent

	var streamInitialised bool = false
	var nextSequenceToken *string
	var messages int = 0

	for {
		timeout := time.After(time.Millisecond * maxBatchWaitMilliseconds)
		var timeoutPassed = false
	COLLECT:
		for {
			if messages < maxBatchSize {
				// must only run this select if we have capacity to receive the message
				select {
				case nextSequenceToken = <-self.streamInitChannel:
					// this is half of logic to send as soon as stream is initialised and timeout passes (see other half below)
					streamInitialised = true
					if timeoutPassed {
						break COLLECT
					} else {
						continue COLLECT
					}
				case m := <-self.channel:
					batch[messages] = m
					messages++
				case <-timeout:
					// this is the other half of the logic to send as soon as stream is initialised and timeout passes
					timeoutPassed = true
					if streamInitialised {
						break COLLECT
					} else {
						continue COLLECT
					}
				}
			} else {
				if streamInitialised {
					// full batch and a stream - send it
					break COLLECT
				} else {
					// all we can do here is wait for the stream to be initialised for as long as it takes
					select {
					case nextSequenceToken = <-self.streamInitChannel:
						streamInitialised = true
						// and then send it
						break COLLECT
					}
				}
			}
		}
		if messages == 0 {
			continue
		}
		// start backoff at 1/8 second
		var interval time.Duration = 1000 / 8
	SEND:
		for {
			putResponse, putErr := self.adapter.cwl.PutLogEvents(&cloudwatchlogs.PutLogEventsInput{
				LogEvents:     batch[:messages],
				LogGroupName:  &self.logGroupName,
				LogStreamName: &self.name,
				SequenceToken: nextSequenceToken,
			})
			if putErr == nil {
				nextSequenceToken = putResponse.NextSequenceToken
				messages = 0
				break
			} else {
				if awsErr, ok := putErr.(awserr.Error); ok && awsErr.Code() == "ThrottlingException" {
					self.adapter.throttlingExceptions++
				} else {
					self.adapter.otherErrors++
				}
				log.Printf(
					"error putting %d log events to stream \"%s\" in log group \"%s\" (backing off for %d): %s",
					messages,
					self.name,
					self.logGroupName,
					interval,
					putErr,
				)
				timeout := time.After(time.Millisecond * interval)
				interval *= 2
				if interval > maxBackoffIntervalMilliseconds {
					interval = maxBackoffIntervalMilliseconds
				}
				for {
					if messages < maxBatchSize {
						// must only run this select if we have capacity to receive the message
						select {
						case m := <-self.channel:
							batch[messages] = m
							messages++
						case <-timeout:
							continue SEND
						}
					} else {
						// buffer is full, so just wait until interval is up
						select {
						case <-timeout:
							continue SEND
						}

					}
				}
			}
		}
	}
}
