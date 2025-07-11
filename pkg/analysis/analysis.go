/*
Copyright 2023 The K8sGPT Authors.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package analysis

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
	openapi_v2 "github.com/google/gnostic/openapiv2"
	"github.com/k8sgpt-ai/k8sgpt/pkg/ai"
	"github.com/k8sgpt-ai/k8sgpt/pkg/analyzer"
	"github.com/k8sgpt-ai/k8sgpt/pkg/cache"
	"github.com/k8sgpt-ai/k8sgpt/pkg/common"
	"github.com/k8sgpt-ai/k8sgpt/pkg/custom"
	"github.com/k8sgpt-ai/k8sgpt/pkg/kubernetes"
	"github.com/k8sgpt-ai/k8sgpt/pkg/util"
	"github.com/schollz/progressbar/v3"
	"github.com/spf13/viper"
)

type Analysis struct {
	Context            context.Context
	Filters            []string
	Client             *kubernetes.Client
	Language           string
	AIClient           ai.IAI
	PromptMap          map[string]string
	Results            []common.Result
	Errors             []string
	Namespace          string
	LabelSelector      string
	Cache              cache.ICache
	Explain            bool
	MaxConcurrency     int
	AnalysisAIProvider string // The name of the AI Provider used for this analysis
	WithDoc            bool
	WithStats          bool
	Stats              []common.AnalysisStats
}

type (
	AnalysisStatus string
	AnalysisErrors []string
)

const (
	StateOK              AnalysisStatus = "OK"
	StateProblemDetected AnalysisStatus = "ProblemDetected"
)

type JsonOutput struct {
	Provider string          `json:"provider"`
	Errors   AnalysisErrors  `json:"errors"`
	Status   AnalysisStatus  `json:"status"`
	Problems int             `json:"problems"`
	Results  []common.Result `json:"results"`
}

func NewAnalysis(
	backend string,
	language string,
	filters []string,
	namespace string,
	labelSelector string,
	noCache bool,
	explain bool,
	maxConcurrency int,
	withDoc bool,
	interactiveMode bool,
	httpHeaders []string,
	withStats bool,
) (*Analysis, error) {
	// Get kubernetes client from viper.
	kubecontext := viper.GetString("kubecontext")
	kubeconfig := viper.GetString("kubeconfig")
	verbose := viper.GetBool("verbose")
	client, err := kubernetes.NewClient(kubecontext, kubeconfig)
	if verbose {
		fmt.Println("Debug: Checking kubernetes client initialization.")
	}
	if err != nil {
		return nil, fmt.Errorf("initialising kubernetes client: %w", err)
	}
	if verbose {
		fmt.Printf("Debug: Kubernetes client initialized, server=%s.\n", client.Config.Host)
	}

	// Load remote cache if it is configured.
	cache, err := cache.GetCacheConfiguration()
	if verbose {
		fmt.Println("Debug: Checking cache configuration.")
	}
	if err != nil {
		return nil, err
	}
	if verbose {
		fmt.Printf("Debug: Cache configuration loaded, type=%s.\n", cache.GetName())
	}

	if noCache {
		cache.DisableCache()
		if verbose {
			fmt.Println("Debug: Cache disabled.")
		}
	}

	a := &Analysis{
		Context:        context.Background(),
		Filters:        filters,
		Client:         client,
		Language:       language,
		Namespace:      namespace,
		LabelSelector:  labelSelector,
		Cache:          cache,
		Explain:        explain,
		MaxConcurrency: maxConcurrency,
		WithDoc:        withDoc,
		WithStats:      withStats,
	}
	if verbose {
		fmt.Print("Debug: Analysis configuration loaded, ")
		fmt.Printf("filters=%v, language=%s, ", filters, language)
		if namespace == "" {
			fmt.Printf("namespace=none, ")
		} else {
			fmt.Printf("namespace=%s, ", namespace)
		}
		if labelSelector == "" {
			fmt.Printf("labelSelector=none, ")
		} else {
			fmt.Printf("labelSelector=%s, ", labelSelector)
		}
		fmt.Printf("explain=%t, maxConcurrency=%d, ", explain, maxConcurrency)
		fmt.Printf("withDoc=%t, withStats=%t.\n", withDoc, withStats)
	}
	if !explain {
		// Return early if AI use was not requested.
		return a, nil
	}

	var configAI ai.AIConfiguration
	if verbose {
		fmt.Println("Debug: Checking AI configuration.")
	}
	if err := viper.UnmarshalKey("ai", &configAI); err != nil {
		return nil, err
	}

	if len(configAI.Providers) == 0 {
		return nil, errors.New("AI provider not specified in configuration. Please run k8sgpt auth")
	}

	// Backend string will have high priority than a default provider
	// Hence, use the default provider only if the backend is not specified by the user.
	if configAI.DefaultProvider != "" && backend == "" {
		backend = configAI.DefaultProvider
		if verbose {
			fmt.Printf("Debug: Using default AI provider %s.\n", backend)
		}
	}

	if backend == "" {
		backend = "openai"
		if verbose {
			fmt.Printf("Debug: Using default AI provider %s.\n", backend)
		}
	}

	var aiProvider ai.AIProvider
	for _, provider := range configAI.Providers {
		if backend == provider.Name {
			aiProvider = provider
			break
		}
	}

	if aiProvider.Name == "" {
		return nil, fmt.Errorf("AI provider %s not specified in configuration. Please run k8sgpt auth", backend)
	}

	if verbose {
		fmt.Printf("Debug: AI configuration loaded, provider=%s, ", backend)
		fmt.Printf("baseUrl=%s, model=%s.\n", aiProvider.BaseURL, aiProvider.Model)
	}

	aiClient := ai.NewClient(aiProvider.Name)
	customHeaders := util.NewHeaders(httpHeaders)
	aiProvider.CustomHeaders = customHeaders
	if verbose {
		fmt.Println("Debug: Checking AI client initialization.")
	}
	if err := aiClient.Configure(&aiProvider); err != nil {
		return nil, err
	}
	// Initialize prompt map with default prompts
	promptMap := make(map[string]string)
	for promptType, promptTemplate := range ai.PromptMap {
		promptMap[promptType] = promptTemplate
	}
	for promptType, customPrompt := range configAI.PromptMap {
		if promptType != "raw" {
			promptMap[promptType] = customPrompt
		}
	}
	if verbose {
		fmt.Println("Debug: AI client initialized.")
	}
	a.AIClient = aiClient
	a.AnalysisAIProvider = aiProvider.Name
	a.PromptMap = promptMap
	return a, nil
}

func (a *Analysis) CustomAnalyzersAreAvailable() bool {
	var customAnalyzers []custom.CustomAnalyzer
	if err := viper.UnmarshalKey("custom_analyzers", &customAnalyzers); err != nil {
		return false
	}
	return len(customAnalyzers) > 0
}

func (a *Analysis) RunCustomAnalysis() {
	var customAnalyzers []custom.CustomAnalyzer
	if err := viper.UnmarshalKey("custom_analyzers", &customAnalyzers); err != nil {
		a.Errors = append(a.Errors, err.Error())
		return
	}

	semaphore := make(chan struct{}, a.MaxConcurrency)
	var wg sync.WaitGroup
	var mutex sync.Mutex
	verbose := viper.GetBool("verbose")
	if verbose {
		if len(customAnalyzers) == 0 {
			fmt.Println("Debug: No custom analyzers found.")
		} else {
			cAnalyzerNames := make([]string, len(customAnalyzers))
			for i, cAnalyzer := range customAnalyzers {
				cAnalyzerNames[i] = cAnalyzer.Name
			}
			fmt.Printf("Debug: Found custom analyzers %v.\n", cAnalyzerNames)
		}
	}
	for _, cAnalyzer := range customAnalyzers {
		wg.Add(1)
		semaphore <- struct{}{}
		go func(analyzer custom.CustomAnalyzer, wg *sync.WaitGroup, semaphore chan struct{}) {
			defer wg.Done()
			canClient, err := custom.NewClient(cAnalyzer.Connection)
			if err != nil {
				mutex.Lock()
				a.Errors = append(a.Errors, fmt.Sprintf("Client creation error for %s analyzer", cAnalyzer.Name))
				mutex.Unlock()
				return
			}
			if verbose {
				fmt.Printf("Debug: %s launched.\n", cAnalyzer.Name)
			}

			result, err := canClient.Run()
			if result.Kind == "" {
				// for custom analyzer name, we must use a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.',
				//and must start and end with an alphanumeric character (e.g. 'example.com',
				//regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')
				result.Kind = cAnalyzer.Name
			}
			if err != nil {
				mutex.Lock()
				a.Errors = append(a.Errors, fmt.Sprintf("[%s] %s", cAnalyzer.Name, err))
				mutex.Unlock()
				if verbose {
					fmt.Printf("Debug: %s completed with errors.\n", cAnalyzer.Name)
				}
			} else {
				mutex.Lock()
				a.Results = append(a.Results, result)
				mutex.Unlock()
				if verbose {
					fmt.Printf("Debug: %s completed without errors.\n", cAnalyzer.Name)
				}
			}
			<-semaphore
		}(cAnalyzer, &wg, semaphore)
	}
	wg.Wait()
}

func (a *Analysis) RunAnalysis() {
	activeFilters := viper.GetStringSlice("active_filters")
	verbose := viper.GetBool("verbose")

	coreAnalyzerMap, analyzerMap := analyzer.GetAnalyzerMap()

	// we get the openapi schema from the server only if required by the flag "with-doc"
	openapiSchema := &openapi_v2.Document{}
	if a.WithDoc {
		var openApiErr error

		if verbose {
			fmt.Println("Debug: Fetching Kubernetes docs.")
		}
		openapiSchema, openApiErr = a.Client.Client.Discovery().OpenAPISchema()
		if verbose {
			fmt.Println("Debug: Checking Kubernetes docs.")
		}
		if openApiErr != nil {
			a.Errors = append(a.Errors, fmt.Sprintf("[KubernetesDoc] %s", openApiErr))
		}
	}

	analyzerConfig := common.Analyzer{
		Client:        a.Client,
		Context:       a.Context,
		Namespace:     a.Namespace,
		LabelSelector: a.LabelSelector,
		AIClient:      a.AIClient,
		OpenapiSchema: openapiSchema,
	}

	// Set a reasonable maximum for concurrency to prevent excessive memory allocation
	const maxAllowedConcurrency = 100
	concurrency := a.MaxConcurrency
	if concurrency <= 0 {
		concurrency = 10 // Default value if not set
	} else if concurrency > maxAllowedConcurrency {
		concurrency = maxAllowedConcurrency // Cap at a reasonable maximum
	}

	semaphore := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mutex sync.Mutex
	// if there are no filters selected and no active_filters then run coreAnalyzer
	if len(a.Filters) == 0 && len(activeFilters) == 0 {
		if verbose {
			fmt.Println("Debug: No filters selected and no active filters found, run all core analyzers.")
		}
		for name, analyzer := range coreAnalyzerMap {
			wg.Add(1)
			semaphore <- struct{}{}
			go a.executeAnalyzer(analyzer, name, analyzerConfig, semaphore, &wg, &mutex)

		}
		wg.Wait()
		return
	}
	// if the filters flag is specified
	if len(a.Filters) != 0 {
		if verbose {
			fmt.Printf("Debug: Filter flags %v specified, run selected core analyzers.\n", a.Filters)
		}
		for _, filter := range a.Filters {
			if analyzer, ok := analyzerMap[filter]; ok {
				semaphore <- struct{}{}
				wg.Add(1)
				go a.executeAnalyzer(analyzer, filter, analyzerConfig, semaphore, &wg, &mutex)
			} else {
				a.Errors = append(a.Errors, fmt.Sprintf("\"%s\" filter does not exist. Please run k8sgpt filters list.", filter))
			}
		}
		wg.Wait()
		return
	}

	// use active_filters
	if len(activeFilters) > 0 && verbose {
		fmt.Printf("Debug: Found active filters %v, run selected core analyzers.\n", activeFilters)
	}
	for _, filter := range activeFilters {
		if analyzer, ok := analyzerMap[filter]; ok {
			semaphore <- struct{}{}
			wg.Add(1)
			go a.executeAnalyzer(analyzer, filter, analyzerConfig, semaphore, &wg, &mutex)
		}
	}
	wg.Wait()
}

func (a *Analysis) executeAnalyzer(analyzer common.IAnalyzer, filter string, analyzerConfig common.Analyzer, semaphore chan struct{}, wg *sync.WaitGroup, mutex *sync.Mutex) {
	defer wg.Done()

	var startTime time.Time
	var elapsedTime time.Duration

	// Start the timer
	if a.WithStats {
		startTime = time.Now()
	}

	// Run the analyzer
	verbose := viper.GetBool("verbose")
	if verbose {
		fmt.Printf("Debug: %s launched.\n", reflect.TypeOf(analyzer).Name())
	}
	results, err := analyzer.Analyze(analyzerConfig)
	if err != nil {
		fmt.Println(err)
	}
	// Measure the time taken
	if a.WithStats {
		elapsedTime = time.Since(startTime)
	}
	stat := common.AnalysisStats{
		Analyzer:     filter,
		DurationTime: elapsedTime,
	}

	mutex.Lock()
	defer mutex.Unlock()

	if err != nil {
		if a.WithStats {
			a.Stats = append(a.Stats, stat)
		}
		a.Errors = append(a.Errors, fmt.Sprintf("[%s] %s", filter, err))
		if verbose {
			fmt.Printf("Debug: %s completed with errors.\n", reflect.TypeOf(analyzer).Name())
		}
	} else {
		if a.WithStats {
			a.Stats = append(a.Stats, stat)
		}
		a.Results = append(a.Results, results...)
		if verbose {
			fmt.Printf("Debug: %s completed without errors.\n", reflect.TypeOf(analyzer).Name())
		}
	}
	<-semaphore
}

func (a *Analysis) GetAIResults(output string, anonymize bool) error {
	if len(a.Results) == 0 {
		return nil
	}

	verbose := viper.GetBool("verbose")
	if verbose {
		fmt.Println("Debug: Generating AI analysis.")
	}

	var bar *progressbar.ProgressBar
	if output != "json" {
		bar = progressbar.Default(int64(len(a.Results)))
	}

	for index, analysis := range a.Results {
		var texts []string

		if bar != nil && verbose {
			bar.Describe(fmt.Sprintf("Analyzing %s", analysis.Kind))
		}

		for _, failure := range analysis.Error {
			if anonymize {
				for _, s := range failure.Sensitive {
					failure.Text = util.ReplaceIfMatch(failure.Text, s.Unmasked, s.Masked)
				}
			}
			texts = append(texts, failure.Text)
		}

		promptTemplate := a.PromptMap["default"]
		// If the resource `Kind` comes from an "integration plugin",
		// maybe a customized prompt template will be involved.
		if prompt, ok := a.PromptMap[analysis.Kind]; ok {
			promptTemplate = prompt
		}
		result, err := a.getAIResultForSanitizedFailures(texts, promptTemplate)
		if err != nil {
			// FIXME: can we avoid checking if output is json multiple times?
			//   maybe implement the progress bar better?
			if output != "json" {
				_ = bar.Exit()
			}

			// Check for exhaustion.
			if strings.Contains(err.Error(), "status code: 429") {
				return fmt.Errorf("exhausted API quota for AI provider %s: %v", a.AIClient.GetName(), err)
			}
			return fmt.Errorf("failed while calling AI provider %s: %v", a.AIClient.GetName(), err)
		}

		if anonymize {
			for _, failure := range analysis.Error {
				for _, s := range failure.Sensitive {
					result = strings.ReplaceAll(result, s.Masked, s.Unmasked)
				}
			}
		}

		analysis.Details = result
		if output != "json" {
			_ = bar.Add(1)
		}
		a.Results[index] = analysis
	}
	return nil
}

func (a *Analysis) getAIResultForSanitizedFailures(texts []string, promptTmpl string) (string, error) {
	inputKey := strings.Join(texts, " ")
	// Check for cached data.
	// TODO(bwplotka): This might depend on model too (or even other client configuration pieces), fix it in later PRs.
	cacheKey := util.GetCacheKey(a.AIClient.GetName(), a.Language, inputKey)

	if !a.Cache.IsCacheDisabled() && a.Cache.Exists(cacheKey) {
		response, err := a.Cache.Load(cacheKey)
		if err != nil {
			return "", err
		}

		if response != "" {
			output, err := base64.StdEncoding.DecodeString(response)
			if err == nil {
				return string(output), nil
			}
			color.Red("error decoding cached data; ignoring cache item: %v", err)
		}
	}

	// Process template.
	prompt := fmt.Sprintf(strings.TrimSpace(promptTmpl), a.Language, inputKey)
	if a.AIClient.GetName() == ai.CustomRestClientName {
		prompt = fmt.Sprintf(ai.PromptMap["raw"], a.Language, inputKey, prompt)
	}
	response, err := a.AIClient.GetCompletion(a.Context, prompt)
	if err != nil {
		return "", err
	}

	if err = a.Cache.Store(cacheKey, base64.StdEncoding.EncodeToString([]byte(response))); err != nil {
		color.Red("error storing value to cache; value won't be cached: %v", err)
	}
	return response, nil
}

func (a *Analysis) Close() {
	if a.AIClient == nil {
		return
	}
	a.AIClient.Close()
}
