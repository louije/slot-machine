# frozen_string_literal: true

require 'json'

module SlotMachine
  # Contract is the app contract from docs/orchestrator-spec.md.
  #
  # Only the fields the spec requires of an orchestrator's core are read here.
  # slot-machine's own config carries a good deal more — an agent, a deploy gate,
  # shared directories — and a conformant implementation is expected to ignore
  # all of it, which this one does. Unknown fields are not an error.
  class Contract
    REQUIRED = %w[start_command port health_endpoint].freeze

    attr_reader :start_command, :setup_command, :port, :internal_port,
                :health_endpoint, :health_timeout_ms, :drain_timeout_ms, :env_file

    def self.load(path)
      raw = JSON.parse(File.read(path))
      new(raw)
    rescue JSON::ParserError => e
      raise ArgumentError, "#{path} is not valid JSON: #{e.message}"
    end

    def initialize(raw)
      missing = REQUIRED.reject { |k| raw[k] && raw[k] != '' }
      raise ArgumentError, "app contract is missing: #{missing.join(', ')}" unless missing.empty?

      @start_command = raw.fetch('start_command')
      @setup_command = raw['setup_command']
      @port = Integer(raw.fetch('port'))
      # An app that serves health checks on its public port is allowed; the
      # separate internal port is a recommendation, not a requirement.
      @internal_port = Integer(raw['internal_port'] || @port)
      @health_endpoint = raw.fetch('health_endpoint')
      @health_timeout_ms = Integer(raw['health_timeout_ms'] || 10_000)
      @drain_timeout_ms = Integer(raw['drain_timeout_ms'] || 5_000)
      @env_file = raw['env_file']

      raise ArgumentError, "health_endpoint #{@health_endpoint.inspect} must start with /" unless @health_endpoint.start_with?('/')
    end

    # env_pairs reads the optional env_file, relative to the repository.
    def env_pairs(repo_dir)
      return {} if @env_file.nil? || @env_file.empty?

      path = @env_file.start_with?('/') ? @env_file : File.join(repo_dir, @env_file)
      return {} unless File.exist?(path)

      File.readlines(path).each_with_object({}) do |line, out|
        line = line.strip
        next if line.empty? || line.start_with?('#') || !line.include?('=')

        key, value = line.split('=', 2)
        out[key] = value
      end
    end
  end
end
