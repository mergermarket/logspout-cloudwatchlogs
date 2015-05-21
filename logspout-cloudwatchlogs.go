package logspout

import (
    "github.com/gliderlabs/logspout/router"
    "github.com/awslabs/aws-sdk-go/aws"
    "github.com/awslabs/aws-sdk-go/service/cloudwatchlogs"
    "github.com/fsouza/go-dockerclient"
    "fmt"
    "strings"
    "log"
    "errors"
    "encoding/json"
)

func init() {
    router.AdapterFactories.Register(NewCloudWatchLogsAdapter, "cloudwatchlogs")
}

type CloudWatchLogsAdapter struct {
    cwl *cloudwatchlogs.CloudWatchLogs
    route *router.Route
    streamPrefix string
    defaultLogGroup string
    logGroupFromEnv bool
    containerLogGroups map[string]LogGroups
    containerStreams map[string]*containerStream
}

type LogGroups struct {
	Both string
	Stderr string
	Stdout string
}

func NewCloudWatchLogsAdapter(route *router.Route) (router.LogAdapter, error) {
    cwl := cloudwatchlogs.New(&aws.Config{
        Region: route.Address,
    })
    for k := range route.Options {
        if k != "stream-prefix" && k != "default-log-group" && k != "log-group-from-env" && k != "container-log-groups" {
            return nil, errors.New(fmt.Sprintf("unknown option %s", k))
        }
    }
	if route.Options["stream-prefix"] == "" {
		return nil, errors.New("stream-prefix is a required option")
	}
	var logGroupFromEnv bool
	logGroupFromEnvParameter, logGroupFromEnvSet := route.Options["log-group-from-env"]
	if ! logGroupFromEnvSet || logGroupFromEnvParameter == "off" {
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
    return &CloudWatchLogsAdapter{
        cwl: cwl,
        route: route,
        streamPrefix: route.Options["stream-prefix"],
        defaultLogGroup: route.Options["default-log-group"],
        logGroupFromEnv: logGroupFromEnv,
        containerLogGroups: containerLogGroups,
        containerStreams: make(map[string]*containerStream),
    }, nil
}

func (self *CloudWatchLogsAdapter) Stream(logstream chan *router.Message) {
    for m := range logstream {
        name := strings.Join([]string{ self.streamPrefix, m.Container.Name[1:], m.Source }, "-")
		id := strings.Join([]string{ m.Container.ID, m.Source }, "-")
        var stream = self.containerStreams[id]
        if stream == nil {
            stream = self.createContainerStream(name, m.Container, m.Source)
            self.containerStreams[id] = stream
        }
        if stream.channel != nil {
            stream.channel <- m
        }
    }
}

func (self *CloudWatchLogsAdapter) createContainerStream (name string, container *docker.Container, source string) *containerStream {
    logGroupName, logGroupNameSource := self.getLogGroupName(container, source)
    var channel chan *router.Message
    if logGroupName != "" {
        channel = make(chan *router.Message)
    }
    stream := containerStream{
        logGroupName: logGroupName,
		logGroupNameSource: logGroupNameSource,
        channel: channel,
        adapter: self,
        seenLogStream: false,
        streamName: name,
    }
    if stream.channel != nil {
        go stream.handleMessages()
    }
    return &stream
}

const envPrefix = "LOGSPOUT_CLOUDWATCHLOGS_LOG_GROUP="
const stdoutEnvPrefix = "LOGSPOUT_CLOUDWATCHLOGS_LOG_GROUP_STDOUT="
const stderrEnvPrefix = "LOGSPOUT_CLOUDWATCHLOGS_LOG_GROUP_STDERR="

func (self *CloudWatchLogsAdapter) getLogGroupName (container *docker.Container, source string) (string, string) {
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
    logGroupName string
	logGroupNameSource string
    streamName string
    channel chan *router.Message
    adapter *CloudWatchLogsAdapter
    seenLogStream bool
    nextSequenceToken *string
}

func (self *containerStream) getNextSequenceToken (m *router.Message) (*string, error) {
    if self.seenLogStream {
        return self.nextSequenceToken, nil
    }
    logStreamsDescription, descErr := self.adapter.cwl.DescribeLogStreams(&cloudwatchlogs.DescribeLogStreamsInput{
        LogGroupName: &self.logGroupName,
        LogStreamNamePrefix: &self.streamName,
    })
    if descErr != nil {
		return nil, errors.New(fmt.Sprintf(
			"could not check for (describe) log stream \"%s\" for log group \"%s\" (from %s): %s",
			self.streamName,
			self.logGroupName,
			self.logGroupNameSource,
			descErr,
		))
    }
    if len(logStreamsDescription.LogStreams) == 0 {
        _, createErr := self.adapter.cwl.CreateLogStream(&cloudwatchlogs.CreateLogStreamInput{
            LogGroupName: &self.logGroupName,
            LogStreamName: &self.streamName,
        })
        if createErr != nil {
			return nil, errors.New(fmt.Sprintf(
				"could not create log stream \"%s\" for log group \"%s\" (from %s): %s",
				self.streamName,
				self.logGroupName,
				self.logGroupNameSource,
				createErr,
			))
        }
        self.seenLogStream = true
        return nil, nil
    } else {
        if *logStreamsDescription.LogStreams[0].LogStreamName != self.streamName {
			return nil, errors.New(fmt.Sprintf("unexpected log stream \"%s\" matching \"%s\" (%d streams returned) in log group \"%s\" (from %s)",
                *logStreamsDescription.LogStreams[0].LogStreamName,
                self.streamName,
                len(logStreamsDescription.LogStreams),
				self.logGroupName,
				self.logGroupNameSource,
            ))
        }
        self.seenLogStream = true
        self.nextSequenceToken = logStreamsDescription.LogStreams[0].UploadSequenceToken
        return self.nextSequenceToken, nil
    }
}

func (self *containerStream) handleMessages () {
    for {
        m := <-self.channel
        sequenceToken, err := self.getNextSequenceToken(m)
        if err != nil {
            // TODO do something more sophisticated than dropping the message on the floor
            // TODO backoff on failure
            log.Print("error getting log stream: ", err)
            continue
        }
        putResponse, putErr := self.adapter.cwl.PutLogEvents(&cloudwatchlogs.PutLogEventsInput{
            LogEvents: []*cloudwatchlogs.InputLogEvent{
                &cloudwatchlogs.InputLogEvent{
                    Message: aws.String(fmt.Sprintf("%s\n", m.Data)),
                    Timestamp: aws.Long(m.Time.UnixNano() / (1000 * 1000)),
                },
            },
            LogGroupName: &self.logGroupName,
            LogStreamName: &self.streamName,
            SequenceToken: sequenceToken,
        })
        if putErr != nil {
            // TODO do something more sophisticated than dropping message on the floor
            // TODO backoff on failure
            log.Print(
				"error putting log events to stream \"", self.streamName, "\" in log group \"", self.logGroupName, "\" ",
				"for ", m.Container.Name[1:], ".", m.Source, ": ", putErr,
			)
            continue
        }
        self.nextSequenceToken = putResponse.NextSequenceToken
    }
}

