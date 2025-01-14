package optimize

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/apex/log"
	"github.com/spf13/cobra"

	"github.com/cisco-open/fsoc/cmd/uql"
	"github.com/cisco-open/fsoc/output"
)

var defaultEvents = []string{
	"optimization_baselined",
	"optimization_started",
	"optimization_ended",
	"stage_started",
	"stage_ended",
	"experiment_started",
	"experiment_ended",
	"experiment_deployment_started",
	"experiment_deployment_completed",
	"experiment_measurement_started",
	"experiment_measurement_completed",
	"experiment_described",
	"recommendation_identified",
	"recommendation_verified",
	"recommendation_invalidated",
}

var progressEvents = []string{
	"optimization_progress",
	"stage_progress",
	"experiment_progress",
}

func init() {
	// TODO move this logic to optimize root when implementing unit tests
	optimizeCmd.AddCommand(NewCmdEvents())
	optimizeCmd.AddCommand(NewCmdRecommendations())
}

type eventsFlags struct {
	clusterId      string
	namespace      string
	workloadName   string
	optimizerId    string
	since          string
	until          string
	count          int
	follow         bool
	followInterval time.Duration
	solutionName   string
}

type eventsCmdFlags struct {
	eventsFlags
	includeProgress bool
	events          []string
}

type EventsRow struct {
	Timestamp       time.Time
	EventAttributes map[string]any
}

type recommendationRow struct {
	EventsRow
	BlockersAttributes map[string]any
	BlockersPresent    string
	Blockers           []string
}

func NewCmdEvents() *cobra.Command {
	var flags eventsCmdFlags
	command := &cobra.Command{
		Use:   "events",
		Short: "Retrieve event logs for a given optimization/workload. Useful for monitoring and debug",
		Example: `  fsoc optimize events
  fsoc optimize events --since -7d until 2023-07-31
  fsoc optimize events --events="experiment_deployment_started,experiment_deployment_completed"
  fsoc optimize events --optimizer-id namespace-name-00000000-0000-0000-0000-000000000000 --count 5
  fsoc optimize events --namespace some-namespace --cluster-id 00000000-0000-0000-0000-000000000000
  fsoc optimize events --workload-name some-workload`,
		RunE:             listEvents(&flags),
		TraverseChildren: true,
		Annotations: map[string]string{
			output.TableFieldsAnnotation:  "OptimizerId: .EventAttributes[\"optimize.optimization.optimizer_id\"], EventType: .EventAttributes[\"appd.event.type\"], Timestamp: .Timestamp",
			output.DetailFieldsAnnotation: "OptimizerId: .EventAttributes[\"optimize.optimization.optimizer_id\"], EventType: .EventAttributes[\"appd.event.type\"], Timestamp: .Timestamp, Attributes: .EventAttributes",
		},
	}

	command.Flags().StringVarP(&flags.clusterId, "cluster-id", "c", "", "Retrieve events constrained to a specific cluster by its ID")
	command.Flags().StringVarP(&flags.namespace, "namespace", "n", "", "Retrieve events constrained to a specific namespace by its name")
	command.Flags().StringVarP(&flags.workloadName, "workload-name", "w", "", "Retrieve events constrained to a specific workload by its name")
	command.Flags().StringVarP(&flags.optimizerId, "optimizer-id", "i", "", "Retrieve events for a specific optimizer by its ID")
	command.MarkFlagsMutuallyExclusive("optimizer-id", "cluster-id")
	command.MarkFlagsMutuallyExclusive("optimizer-id", "namespace")
	command.MarkFlagsMutuallyExclusive("optimizer-id", "workload-name")

	command.Flags().BoolVarP(&flags.includeProgress, "include-progress", "p", false, "Include progress events in query and output")
	command.Flags().StringSliceVarP(&flags.events, "events", "e", defaultEvents, "Customize the types of events to be retrieved")
	command.MarkFlagsMutuallyExclusive("include-progress", "events")

	command.Flags().StringVarP(&flags.since, "since", "s", "", "Retrieve events contained in the time interval starting at a relative or exact time. (default: -1h)")
	command.Flags().StringVarP(&flags.until, "until", "u", "", "Retrieve events contained in the time interval ending at a relative or exact time. (default: now)")
	command.Flags().IntVarP(&flags.count, "count", "", -1, "Limit the number of events retrieved to the specified count")

	command.Flags().BoolVarP(&flags.follow, "follow", "f", false, "Follow the events as they are produced")
	command.Flags().DurationVarP(&flags.followInterval, "follow-interval", "t", time.Second*60, "Duration between requests to UQL when following events")
	command.MarkFlagsMutuallyExclusive("follow", "count")

	command.Flags().StringVarP(&flags.solutionName, "solution-name", "", "optimize", "Intended for developer usage, overrides the name of the solution defining the FMM types for reading")
	if err := command.LocalFlags().MarkHidden("solution-name"); err != nil {
		log.Warnf("Failed to set events solution-name flag hidden: %v", err)
	}

	return command
}

type eventsTemplateValues struct {
	Since  string
	Until  string
	Events string
	Filter string
	Limits string
}

var eventsTemplate = template.Must(template.New("").Parse(`
{{ with .Since }}SINCE {{ . }}
{{ end -}}
{{ with .Until }}UNTIL {{ . }}
{{ end -}}
FETCH events(
		{{ .Events }}
	)
	{{ with .Filter }}[{{ . }}]
	{{ end -}}
	{attributes, timestamp}
{{ with .Limits }}LIMITS events.count({{ . }})
{{ end -}}
ORDER events.asc()
`))

func listEvents(flags *eventsCmdFlags) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		// setup query
		tempVals := eventsTemplateValues{
			Since: flags.since,
			Until: flags.until,
		}

		if flags.includeProgress {
			flags.events = append(flags.events, progressEvents...)
		}
		fullyQualifiedEvents := make([]string, 0, len(flags.events))
		for _, value := range flags.events {
			fullyQualifiedEvents = append(fullyQualifiedEvents, fmt.Sprintf("%v:%v", flags.solutionName, value))
		}
		tempVals.Events = strings.Join(fullyQualifiedEvents, ",\n		")

		filterList := make([]string, 0, 2)
		if flags.clusterId != "" {
			filterList = append(filterList, fmt.Sprintf("attributes(k8s.cluster.id) = %q", flags.clusterId))
		}
		if flags.optimizerId != "" {
			filterList = append(filterList, fmt.Sprintf("attributes(optimize.optimization.optimizer_id) = %q", flags.optimizerId))
		} else if flags.namespace != "" || flags.workloadName != "" {
			optimizerIds, err := listOptimizations(&flags.eventsFlags)
			if err != nil {
				return fmt.Errorf("listOptimizations: %w", err)
			}
			if len(optimizerIds) < 1 {
				output.PrintCmdStatus(cmd, "No optimization entities found matching the given criteria\n")
				return nil
			}
			optIdStr := strings.Join(optimizerIds, "\", \"")
			filterList = append(filterList, fmt.Sprintf("attributes(optimize.optimization.optimizer_id) IN [\"%v\"]", optIdStr))
		}
		tempVals.Filter = strings.Join(filterList, " && ")

		if flags.count != -1 {
			if flags.count > 1000 {
				return errors.New("counts higher than 1000 are not supported")
			}
			tempVals.Limits = strconv.Itoa(flags.count)
		}

		var buff bytes.Buffer
		if err := eventsTemplate.Execute(&buff, tempVals); err != nil {
			return fmt.Errorf("eventsTemplate.Execute: %w", err)
		}
		query := buff.String()

		// execute query, process results
		resp, err := uql.ClientV1.ExecuteQuery(&uql.Query{Str: query})
		if err != nil {
			return fmt.Errorf("uql.ClientV1.ExecuteQuery: %w", err)
		}
		if resp.HasErrors() {
			log.Error("Execution of events query encountered errors. Returned data may not be complete!")
			for _, e := range resp.Errors() {
				log.Errorf("%s: %s", e.Title, e.Detail)
			}
		}

		main_data_set := resp.Main()
		if main_data_set == nil || len(main_data_set.Data) < 1 {
			output.PrintCmdStatus(cmd, "No event results found for given input\n")
			return nil
		}
		if len(main_data_set.Data[0]) < 1 {
			return fmt.Errorf("main dataset %v first row has no columns", main_data_set.Name)
		}

		data_set, ok := main_data_set.Data[0][0].(*uql.DataSet)
		if !ok {
			return fmt.Errorf("main dataset %v first row first column (type %T) could not be converted to *uql.DataSet", main_data_set.Name, main_data_set.Data[0][0])
		}
		eventRows, err := extractEventsData(data_set)
		if err != nil {
			return fmt.Errorf("extractEventsData: %w", err)
		}

		// handle pagination
		next_ok := false
		if data_set != nil {
			_, next_ok = data_set.Links["next"]
		}
		if flags.count != -1 {
			// skip pagination if limits provided. Otherwise, we return the full result list (chunked into count per response)
			// instead of constraining to count
			next_ok = false
		}
		if flags.follow {
			// skip next cursor pagination on follow since the follow cursor contains the same data
			next_ok = false
		}
		for page := 2; next_ok; page++ {
			resp, err = uql.ClientV1.ContinueQuery(data_set, "next")
			if err != nil {
				return fmt.Errorf("page %v uql.ClientV1.ContinueQuery: %w", page, err)
			}
			if resp.HasErrors() {
				log.Errorf("Continuation of events query (page %v) encountered errors. Returned data may not be complete!", page)
				for _, e := range resp.Errors() {
					log.Errorf("%s: %s", e.Title, e.Detail)
				}
			}
			main_data_set := resp.Main()
			if main_data_set == nil {
				log.Errorf("Continuation of events query (page %v) has nil main data. Returned data may not be complete!", page)
				break
			}
			if len(main_data_set.Data) < 1 {
				return fmt.Errorf("page %v main dataset %v has no rows", page, main_data_set.Name)
			}
			if len(main_data_set.Data[0]) < 1 {
				return fmt.Errorf("page %v main dataset %v first row has no columns", page, main_data_set.Name)
			}
			data_set, ok = main_data_set.Data[0][0].(*uql.DataSet)
			if !ok {
				return fmt.Errorf("page %v main dataset %v first row first column (type %T) could not be converted to *uql.DataSet", page, main_data_set.Name, main_data_set.Data[0][0])
			}

			newRows, err := extractEventsData(data_set)
			if err != nil {
				return fmt.Errorf("page %v extractEventsData: %w", page, err)
			}
			eventRows = append(eventRows, newRows...)
			_, next_ok = data_set.Links["next"]
		}

		output.PrintCmdOutput(cmd, struct {
			Items []EventsRow `json:"items"`
			Total int         `json:"total"`
		}{Items: eventRows, Total: len(eventRows)})

		// handle follow
		if flags.follow && data_set != nil {
			// setup async channels
			interrupt := make(chan os.Signal, 1)
			signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
			followChan := make(chan *followEventResult, 1)
			followChan <- &followEventResult{data_set: data_set}

			for {
				select {
				case <-interrupt:
					// exit requested
					return nil
				case followResult := <-followChan:
					if followResult.err != nil {
						return followResult.err
					}
					// queue up next follow interval sleep and print
					// run in background to allow interrupts
					go func() {
						// Return immediately available results (additional pages) right away.
						// Don't start waiting until follow cursor returns a response smaller than the max page size.
						if followResult.cursorExhausted {
							time.Sleep(flags.followInterval)
						}
						followChan <- followDatasetAndPrint(cmd, followResult.data_set)
					}()
				}
			}
		}

		return nil
	}
}

type followEventResult struct {
	data_set        *uql.DataSet
	err             error
	cursorExhausted bool
}

func followDatasetAndPrint(cmd *cobra.Command, data_set *uql.DataSet) *followEventResult {
	resp, err := uql.ClientV1.ContinueQuery(data_set, "follow")
	if err != nil {
		return &followEventResult{err: fmt.Errorf("follow uql.ClientV1.ContinueQuery: %w", err)}
	}
	if resp.HasErrors() {
		log.Error("Following of events query encountered errors. Returned data may not be complete!")
		for _, e := range resp.Errors() {
			log.Errorf("%s: %s", e.Title, e.Detail)
		}
	}
	main_data_set := resp.Main()
	if main_data_set == nil {
		log.Error("Following of events query has nil main data. Returned data may not be complete!")
		return &followEventResult{data_set: data_set}
	}
	if len(main_data_set.Data) < 1 {
		return &followEventResult{err: fmt.Errorf("follow main dataset %v has no rows", main_data_set.Name)}
	}
	if len(main_data_set.Data[0]) < 1 {
		return &followEventResult{err: fmt.Errorf("follow main dataset %v first row has no columns", main_data_set.Name)}
	}
	var ok bool
	data_set, ok = main_data_set.Data[0][0].(*uql.DataSet)
	if !ok {
		return &followEventResult{err: fmt.Errorf("follow main dataset %v first row first column (type %T) could not be converted to *uql.DataSet", main_data_set.Name, main_data_set.Data[0][0])}
	}

	result := &followEventResult{data_set: data_set}
	newRows, err := extractEventsData(data_set)
	if err != nil {
		result.err = fmt.Errorf("follow extractEventsData: %w", err)
		return result
	}

	newRowsCount := len(newRows)
	if newRowsCount > 0 {
		output.PrintCmdOutputCustom(cmd, struct {
			Items []EventsRow `json:"items"`
			Total int         `json:"total"`
		}{Items: newRows, Total: newRowsCount}, &output.Table{OmitHeaders: true})
	} else {
		result.cursorExhausted = true
	}
	return result
}

type recommendationsCmdFlags struct {
	eventsFlags
	includeInvalidated bool
}

func NewCmdRecommendations() *cobra.Command {
	var flags recommendationsCmdFlags
	command := &cobra.Command{
		Use:   "recommendations",
		Short: "Retrieve resulting recommendations for a given optimization/workload",
		Example: `  fsoc optimize recommendations --optimizer-id namespace-name-00000000-0000-0000-0000-000000000000
  fsoc optimize recommendations --optimizer-id namespace-name-00000000-0000-0000-0000-000000000000 --include-invalidated --count 5`,
		RunE:             listRecommendations(&flags),
		TraverseChildren: true,
		Annotations: map[string]string{
			output.TableFieldsAnnotation:  "OptimizerId: .EventAttributes[\"optimize.optimization.optimizer_id\"], State: .EventAttributes[\"optimize.recommendation.state\"], CPUcores: .EventAttributes[\"optimize.recommendation.settings.cpu\"], MemoryGiB: .EventAttributes[\"optimize.recommendation.settings.memory\"], Blockers: .BlockersPresent, Timestamp: .Timestamp",
			output.DetailFieldsAnnotation: "OptimizerId: .EventAttributes[\"optimize.optimization.optimizer_id\"], State: .EventAttributes[\"optimize.recommendation.state\"], CPUcores: .EventAttributes[\"optimize.recommendation.settings.cpu\"], MemoryGiB: .EventAttributes[\"optimize.recommendation.settings.memory\"], Blockers: .Blockers, BlockersAttributes: .BlockersAttributes, Attributes: .EventAttributes, Timestamp: .Timestamp",
		},
	}

	command.Flags().StringVarP(&flags.clusterId, "cluster-id", "c", "", "Retrieve recommendations constrained to a specific cluster by its ID")
	command.Flags().StringVarP(&flags.namespace, "namespace", "n", "", "Retrieve recommendations constrained to a specific namespace by its name")
	command.Flags().StringVarP(&flags.workloadName, "workload-name", "w", "", "Retrieve recommendations constrained to a specific workload by its name")
	command.Flags().StringVarP(&flags.optimizerId, "optimizer-id", "i", "", "Retrieve recommendations for a specific optimizer by its ID")
	command.MarkFlagsMutuallyExclusive("optimizer-id", "cluster-id")
	command.MarkFlagsMutuallyExclusive("optimizer-id", "namespace")
	command.MarkFlagsMutuallyExclusive("optimizer-id", "workload-name")

	command.Flags().BoolVarP(&flags.includeInvalidated, "include-invalidated", "", false, "Include recommendations that have not been verified")

	command.Flags().StringVarP(&flags.since, "since", "s", "-52w", "Retrieve recommendations contained in the time interval starting at a relative or exact time.")
	command.Flags().StringVarP(&flags.until, "until", "u", "", "Retrieve recommendations contained in the time interval ending at a relative or exact time. (default: now)")

	command.Flags().IntVarP(&flags.count, "count", "", 1, "Limit the number of recommendations retrieved to the specified count")

	command.Flags().StringVarP(&flags.solutionName, "solution-name", "", "optimize", "Intended for developer usage, overrides the name of the solution defining the FMM types for reading")
	if err := command.LocalFlags().MarkHidden("solution-name"); err != nil {
		log.Warnf("Failed to set recommendations solution-name flag hidden: %v", err)
	}

	return command
}

type recommendationsTemplateValues struct {
	Since              string
	Until              string
	IncludeInvalidated bool
	Filter             string
	Limits             string
	SolutionName       string
}

var recommendationsTemplate = template.Must(template.New("").Parse(`
{{ with .Since }}SINCE {{ . }}
{{ end -}}
{{ with .Until }}UNTIL {{ . }}
{{ end -}}
FETCH events(
		{{- if .IncludeInvalidated }}
		{{ .SolutionName }}:recommendation_identified,
		{{ .SolutionName }}:recommendation_invalidated,
		{{- end }}
		{{ .SolutionName }}:recommendation_verified
	)
	{{ with .Filter }}[{{ . }}]
	{{ end -}}
	{attributes, timestamp}
{{ with .Limits }}LIMITS events.count({{ . }})
{{ end -}}
ORDER events.asc()
`))

var optimizationStartedTemplate = template.Must(template.New("").Parse(`
{{ with .Since }}SINCE {{ . }}
{{ end -}}
{{ with .Until }}UNTIL {{ . }}
{{ end -}}
FETCH events(
		{{ .SolutionName }}:optimization_started
	)
	{{ with .Filter }}[{{ . }}]
	{{ end -}}
	{attributes, timestamp}
ORDER events.asc()
`))

func listRecommendations(flags *recommendationsCmdFlags) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		// setup query
		tempVals := recommendationsTemplateValues{
			Since:              flags.since,
			Until:              flags.until,
			IncludeInvalidated: flags.includeInvalidated,
			SolutionName:       flags.solutionName,
		}

		filterList := make([]string, 0, 2)
		if flags.clusterId != "" {
			filterList = append(filterList, fmt.Sprintf("attributes(k8s.cluster.id) = %q", flags.clusterId))
		}
		if flags.optimizerId != "" {
			filterList = append(filterList, fmt.Sprintf("attributes(optimize.optimization.optimizer_id) = %q", flags.optimizerId))
		} else if flags.namespace != "" || flags.workloadName != "" {
			optimizerIds, err := listOptimizations(&flags.eventsFlags)
			if err != nil {
				return fmt.Errorf("listOptimizations: %w", err)
			}
			if len(optimizerIds) < 1 {
				output.PrintCmdStatus(cmd, "No optimization entities found matching the given criteria\n")
				return nil
			}
			optIdStr := strings.Join(optimizerIds, "\", \"")
			filterList = append(filterList, fmt.Sprintf("attributes(optimize.optimization.optimizer_id) IN [\"%v\"]", optIdStr))
		}
		tempVals.Filter = strings.Join(filterList, " && ")

		if flags.count != -1 {
			if flags.count > 1000 {
				return errors.New("counts higher than 1000 are not supported")
			}
			tempVals.Limits = strconv.Itoa(flags.count)
		}

		var buff bytes.Buffer
		if err := recommendationsTemplate.Execute(&buff, tempVals); err != nil {
			return fmt.Errorf("recommendationsTemplate.Execute: %w", err)
		}
		query := buff.String()

		// execute query, process results
		resp, err := uql.ClientV1.ExecuteQuery(&uql.Query{Str: query})
		if err != nil {
			return fmt.Errorf("uql.ClientV1.ExecuteQuery: %w", err)
		}
		if resp.HasErrors() {
			log.Error("Execution of recommendations query encountered errors. Returned data may not be complete!")
			for _, e := range resp.Errors() {
				log.Errorf("%s: %s", e.Title, e.Detail)
			}
		}

		main_data_set := resp.Main()
		if main_data_set == nil || len(main_data_set.Data) < 1 {
			output.PrintCmdStatus(cmd, "No recommendation results found for given input\n")
			return nil
		}
		if len(main_data_set.Data[0]) < 1 {
			return fmt.Errorf("main dataset %v first row has no columns", main_data_set.Name)
		}

		data_set, ok := main_data_set.Data[0][0].(*uql.DataSet)
		if !ok {
			return fmt.Errorf("main dataset %v first row first column (type %T) could not be converted to *uql.DataSet", main_data_set.Name, main_data_set.Data[0][0])
		}
		recommendationRows, err := extractEventsData(data_set)
		if err != nil {
			return fmt.Errorf("extractEventsData: %w", err)
		}

		// handle pagination
		next_ok := false
		if data_set != nil {
			_, next_ok = data_set.Links["next"]
		}
		if flags.count != -1 {
			// skip pagination if limits provided. Otherwise, we return the full result list (chunked into count per response)
			// instead of constraining to count
			next_ok = false
		}
		for page := 2; next_ok; page++ {
			resp, err = uql.ClientV1.ContinueQuery(data_set, "next")
			if err != nil {
				return fmt.Errorf("page %v uql.ClientV1.ContinueQuery: %w", page, err)
			}
			if resp.HasErrors() {
				log.Errorf("Continuation of recommendations query (page %v) encountered errors. Returned data may not be complete!", page)
				for _, e := range resp.Errors() {
					log.Errorf("%s: %s", e.Title, e.Detail)
				}
			}
			main_data_set := resp.Main()
			if main_data_set == nil {
				log.Errorf("Continuation of recommendations query (page %v) has nil main data. Returned data may not be complete!", page)
				break
			}
			if len(main_data_set.Data) < 1 {
				return fmt.Errorf("page %v main dataset %v has no rows", page, main_data_set.Name)
			}
			if len(main_data_set.Data[0]) < 1 {
				return fmt.Errorf("page %v main dataset %v first row has no columns", page, main_data_set.Name)
			}
			data_set, ok = main_data_set.Data[0][0].(*uql.DataSet)
			if !ok {
				return fmt.Errorf("page %v main dataset %v first row first column (type %T) could not be converted to *uql.DataSet", page, main_data_set.Name, main_data_set.Data[0][0])
			}

			newRows, err := extractEventsData(data_set)
			if err != nil {
				return fmt.Errorf("page %v extractEventsData: %w", page, err)
			}
			recommendationRows = append(recommendationRows, newRows...)
			_, next_ok = data_set.Links["next"]
		}

		recommendationRowsWithBlockers := make([]recommendationRow, 0, len(recommendationRows))

		// extract blocker rows
		blockerRows, err := getOptimizationBlockerData(tempVals)
		if err != nil {
			return fmt.Errorf("failed to retrieve optimization_started blocker data: %v", err)
		}

		// iterate through recommendations rows and append blocker data from optimization_started events, linking on optimizer ID + num
		for i := range recommendationRows {
			optimizerId := recommendationRows[i].EventAttributes["optimize.optimization.optimizer_id"]
			optimizationNum := recommendationRows[i].EventAttributes["optimize.optimization.num"]
			uniqueKey := fmt.Sprintf("%s-%s", optimizerId.(string), optimizationNum.(string))

			recommendationWithBlockers := recommendationRow{}
			recommendationWithBlockers.EventsRow = recommendationRows[i]
			recommendationWithBlockers.BlockersAttributes = make(map[string]any)

			recommendationWithBlockers.BlockersPresent = "false"

			// merge recommendation and blocker data
			if startedRow, ok := blockerRows[uniqueKey]; !ok {
				log.Warnf("No optimization_started event found for recommendation with optimizer_id: %v and num: %v", optimizerId, optimizationNum)
			} else {
				for attr, val := range startedRow.(map[string]any) {
					recommendationWithBlockers.BlockersAttributes[attr] = val

					// extract the ID from the attribute string
					if !strings.Contains(attr, "principal") {
						splitAttr := strings.Split(attr, ".")
						if len(splitAttr) > 3 {
							blockerID := splitAttr[len(splitAttr)-2]
							if !strings.Contains(strings.Join(recommendationWithBlockers.Blockers, ","), blockerID) {
								recommendationWithBlockers.Blockers = append(recommendationWithBlockers.Blockers, blockerID)
							}
						}
					}
				}
			}

			if len(recommendationWithBlockers.Blockers) > 0 {
				recommendationWithBlockers.BlockersPresent = "true"
			}

			recommendationRowsWithBlockers = append(recommendationRowsWithBlockers, recommendationWithBlockers)
		}

		output.PrintCmdOutput(cmd, struct {
			Items []recommendationRow `json:"items"`
			Total int                 `json:"total"`
		}{Items: recommendationRowsWithBlockers, Total: len(recommendationRowsWithBlockers)})

		return nil
	}
}

func getOptimizationBlockerData(tempVals recommendationsTemplateValues) (map[string]any, error) {

	var buff bytes.Buffer
	if err := optimizationStartedTemplate.Execute(&buff, tempVals); err != nil {
		return nil, fmt.Errorf("optimizationStartedTemplate.Execute: %w", err)
	}
	query := buff.String()

	// execute query, process results
	resp, err := uql.ClientV1.ExecuteQuery(&uql.Query{Str: query})
	if err != nil {
		return nil, fmt.Errorf("uql.ExecuteQuery: %w", err)
	}
	if resp.HasErrors() {
		log.Error("Execution of optimization_started query encountered errors. Returned data may not be complete!")
		for _, e := range resp.Errors() {
			log.Errorf("%s: %s", e.Title, e.Detail)
		}
	}

	main_data_set := resp.Main()
	if main_data_set == nil || len(main_data_set.Data) < 1 {
		return nil, fmt.Errorf("no optimization_started results found for given input")
	}
	if len(main_data_set.Data[0]) < 1 {
		return nil, fmt.Errorf("main dataset %v first row has no columns", main_data_set.Name)
	}

	data_set, ok := main_data_set.Data[0][0].(*uql.DataSet)
	if !ok {
		return nil, fmt.Errorf("main dataset %v first row first column (type %T) could not be converted to *uql.DataSet", main_data_set.Name, main_data_set.Data[0][0])
	}
	startedBlockersData, err := extractStartedBlockersData(data_set)
	if err != nil {
		return nil, fmt.Errorf("extractStartedBlockersData: %w", err)
	}

	return startedBlockersData, nil
}

func extractStartedBlockersData(dataset *uql.DataSet) (map[string]any, error) {

	results := make(map[string]any)
	if dataset == nil {
		return results, nil
	}
	resp_data := &dataset.Data

	for _, row := range *resp_data {

		attributes := row[0].(uql.ComplexData)
		attributesMap, _ := sliceToMap(attributes.Data)
		newAttributes := make(map[string]any)

		for attr, val := range attributesMap {
			if strings.HasPrefix(attr, "optimize.ignored_blockers") {
				newAttributes[attr] = val
			}
		}
		uniqueKey := fmt.Sprintf("%s-%s", attributesMap["optimize.optimization.optimizer_id"].(string), attributesMap["optimize.optimization.num"].(string))
		results[uniqueKey] = newAttributes
	}

	return results, nil
}

func extractEventsData(dataset *uql.DataSet) ([]EventsRow, error) {
	if dataset == nil {
		return []EventsRow{}, nil
	}
	resp_data := &dataset.Data
	results := make([]EventsRow, 0, len(*resp_data))

	for _, row := range *resp_data {
		attributes := row[0].(uql.ComplexData)
		attributesMap, _ := sliceToMap(attributes.Data)
		timestamp := row[1].(time.Time)
		results = append(results, EventsRow{Timestamp: timestamp, EventAttributes: attributesMap})
	}

	return results, nil
}

type optimizationTemplateValues struct {
	Since        string
	Until        string
	SolutionName string
	Filter       string
}

var optimizationTemplate = template.Must(template.New("").Parse(`
{{ with .Since }}SINCE {{ . }}
{{ end -}}
{{ with .Until }}UNTIL {{ . }}
{{ end -}}
FETCH attributes(optimize.optimization.optimizer_id)
FROM entities({{ .SolutionName }}:optimization)[{{ .Filter }}]
`))

// listOptimizations takes applicable filter criteria from the eventsFlags and returns a list of applicable optimizer IDs
// from the FMM entity optimize:optimization
func listOptimizations(flags *eventsFlags) ([]string, error) {
	tempVals := optimizationTemplateValues{
		Since:        flags.since,
		Until:        flags.until,
		SolutionName: flags.solutionName,
	}

	filterList := make([]string, 0, 3)
	if flags.namespace != "" {
		filterList = append(filterList, fmt.Sprintf("attributes(\"k8s.namespace.name\") = %q", flags.namespace))
	}
	if flags.workloadName != "" {
		filterList = append(filterList, fmt.Sprintf("attributes(\"k8s.workload.name\") = %q", flags.workloadName))
	}
	if len(filterList) < 1 {
		return []string{}, errors.New("sanity check failed, optimizations query must at least filter on namespace or workload name, otherwise this query can be skipped")
	}
	if flags.clusterId != "" {
		filterList = append(filterList, fmt.Sprintf("attributes(\"k8s.cluster.id\") = %q", flags.clusterId))
	}
	tempVals.Filter = strings.Join(filterList, " && ")

	var buff bytes.Buffer
	if err := optimizationTemplate.Execute(&buff, tempVals); err != nil {
		return []string{}, fmt.Errorf("optimizationTemplate.Execute: %w", err)
	}
	query := buff.String()

	resp, err := uql.ClientV1.ExecuteQuery(&uql.Query{Str: query})
	if err != nil {
		return []string{}, fmt.Errorf("uql.ClientV1.ExecuteQuery: %w", err)
	}
	if resp.HasErrors() {
		log.Error("Execution of optimization query encountered errors. Returned data may not be complete!")
		for _, e := range resp.Errors() {
			log.Errorf("%s: %s", e.Title, e.Detail)
		}
	}

	mainDataSet := resp.Main()
	if mainDataSet == nil {
		return []string{}, nil
	}
	results := make([]string, 0, len(mainDataSet.Data))
	for index, row := range mainDataSet.Data {
		if len(row) < 1 {
			return results, fmt.Errorf("optimization data row %v has no columns", index)
		}
		idStr, ok := row[0].(string)
		if !ok {
			return results, fmt.Errorf("optimization data row %v value %v (type %T) could not be converted to string", index, row[0], row[0])
		}
		results = append(results, idStr)
	}

	_, next_ok := mainDataSet.Links["next"]
	for page := 2; next_ok; page++ {
		resp, err = uql.ClientV1.ContinueQuery(mainDataSet, "next")
		if err != nil {
			return results, fmt.Errorf("page %v uql.ClientV1.ContinueQuery: %w", page, err)
		}

		if resp.HasErrors() {
			log.Errorf("Continuation of optimization query (page %v) encountered errors. Returned data may not be complete!", page)
			for _, e := range resp.Errors() {
				log.Errorf("%s: %s", e.Title, e.Detail)
			}
		}
		mainDataSet = resp.Main()
		if mainDataSet == nil {
			log.Errorf("Continuation of optimization query (page %v) has nil main data. Returned data may not be complete!", page)
			break
		}

		for index, row := range mainDataSet.Data {
			if len(row) < 1 {
				return results, fmt.Errorf("page %v optimization data row %v has no columns", page, index)
			}
			idStr, ok := row[0].(string)
			if !ok {
				return results, fmt.Errorf("page %v optimization data row %v value %v (type %T) could not be converted to string", page, index, row[0], row[0])
			}
			results = append(results, idStr)
		}

		_, next_ok = mainDataSet.Links["next"]
	}

	return results, nil
}
